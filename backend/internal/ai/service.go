package ai

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Service orchestrates AI analysis for a single project. It composes a generic
// API Client with a project-specific Module, the composed system prompt, and a
// snapshot of consecutive failure counts.
type Service struct {
	client         *Client
	module         Module
	systemPrompt   string
	consecutiveMap map[string]int

	// agenticOpts is the resolved agentic config. When Enabled is false
	// (the default), Analyze never goes agentic regardless of any other
	// signal.
	agenticOpts AgenticOptions
	agenticOn   bool
	agenticAll  bool // when true, every failure goes agentic; else module decides

	// browserFactory provides per-build Browser instances. nil when
	// agentic mode is off, in which case Analyze ignores it.
	browserFactory artifacts.Factory

	// toolsUnsupported is set after the first agentic call that returns
	// ErrToolsUnsupported, so subsequent failures fall straight back to
	// the curator pipeline without retrying tools against a provider that
	// will reject them.
	toolsUnsupported atomic.Bool
}

// NewService constructs a Service. systemPrompt is the fully composed prompt
// (engine base + consumer addendum + response format footer) and must be
// non-empty. consecutiveMap is keyed by consecutiveKey(jobID, testName); it
// may be nil.
func NewService(client *Client, module Module, systemPrompt string, consecutiveMap map[string]int) *Service {
	if consecutiveMap == nil {
		consecutiveMap = map[string]int{}
	}
	return &Service{
		client:         client,
		module:         module,
		systemPrompt:   systemPrompt,
		consecutiveMap: consecutiveMap,
	}
}

// EnableAgentic switches the Service into mixed curator + agentic mode. opts
// is the resolved AgenticOptions (typically project.Agentic.EffectiveAgentic()
// translated to the engine struct). factory provides per-build artifact
// Browsers. always=true forces every failure through agentic mode; always=false
// defers the choice to the Module's AgenticPreferrer (modules that don't
// implement it stay on curator).
//
// Safe to call once at Service construction; not safe for concurrent use.
func (s *Service) EnableAgentic(opts AgenticOptions, factory artifacts.Factory, always bool) {
	s.agenticOn = true
	s.agenticAll = always
	s.agenticOpts = opts
	s.browserFactory = factory
}

// Module returns the underlying Module (mainly for logging).
func (s *Service) Module() Module { return s.module }

// Analyze fills tc.AISummary and tc.AIAnalysis for a single failed test case.
// jobID is the stable per-job identifier used to scope the consecutive map
// and agentic cache key so same-named jobs across repos do not share state.
// buildPrefix is the bucket-relative path to the build root (e.g.
// "logs/<job>/<id>/" for periodics or
// "pr-logs/pull/<org_repo>/<pr#>/<job>/<id>/" for presubmits); the agentic
// pipeline scopes its file browser to this prefix.
//
// Behavior:
//   - skip entirely if both AISummary and AIAnalysis are already populated AND
//     the cached analysis's Mode matches the currently desired mode
//   - if the module flags the failure as a known transient, set AISummary with
//     the reason (IsTransient=true) and return without running the API
//   - run the agentic pipeline when enabled AND (always-on OR the module's
//     AgenticPreferrer opts in), otherwise run the single-shot curator pipeline
//   - on API failure, leave an "AI analysis unavailable" summary so the UI
//     still has something to render
func (s *Service) Analyze(ctx context.Context, httpClient *http.Client, jobID, buildPrefix string, run *models.BuildResult, tc *models.TestCase) {
	desiredMode := s.desiredMode(run, tc)

	if tc.AISummary != nil && tc.AIAnalysis != nil && !s.shouldReanalyze(tc, desiredMode) {
		// Stamp a non-empty Mode on legacy cached analyses so the published
		// JSON is uniform. shouldReanalyze treated empty as curator above, so
		// curator is the only value that can land here.
		if tc.AIAnalysis.Mode == "" {
			tc.AIAnalysis.Mode = curatorMode
		}
		return
	}

	if reason := s.module.IsKnownTransient(tc.FailureMessage); reason != "" {
		tc.AISummary = &models.AISummary{
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			Summary:     reason,
			IsTransient: true,
		}
		log.Printf("  ⏭ Skipping transient: %s: %s", tc.Name, reason)
		return
	}

	log.Printf("  🔍 Analyzing: %s [%s]", tc.Name, desiredMode)
	consec := s.consecutiveMap[consecutiveKey(jobID, tc.Name)]
	if consec < 1 {
		consec = 1
	}

	userPrompt := s.module.AnalysisPrompt(ctx, httpClient, run, tc, consec)

	if desiredMode == AgenticMode {
		summary, analysis, err := s.runAgentic(ctx, jobID, buildPrefix, run, tc, userPrompt)
		if err == nil {
			tc.AISummary = summary
			tc.AIAnalysis = analysis
			return
		}
		if errors.Is(err, ErrToolsUnsupported) {
			s.toolsUnsupported.Store(true)
			log.Printf("  ⚠ AI endpoint rejected tools; falling back to curator for this run: %v", err)
			// Fall through to curator path below.
		} else {
			log.Printf("  ⚠ Agentic AI analysis failed for %s: %v", tc.Name, err)
			s.setUnavailable(tc, err)
			return
		}
	}

	summary, analysis, err := s.client.doAnalyze(ctx,
		s.cacheKey(tc.Name, tc.FailureMessage),
		s.systemPrompt,
		userPrompt,
	)
	if err != nil {
		log.Printf("  ⚠ AI analysis failed for %s: %v", tc.Name, err)
		s.setUnavailable(tc, err)
		return
	}
	tc.AISummary = summary
	tc.AIAnalysis = analysis
}

// runAgentic does the per-failure agentic call setup (browser construction +
// cache key + call). Kept separate so Analyze stays readable.
func (s *Service) runAgentic(ctx context.Context, jobID, buildPrefix string, run *models.BuildResult, tc *models.TestCase, userPrompt string) (*models.AISummary, *models.AIAnalysis, error) {
	if s.browserFactory == nil {
		return nil, nil, fmt.Errorf("agentic mode enabled but no browser factory configured")
	}
	browser := s.browserFactory.ForBuild(buildPrefix, run.JobName+"/"+run.BuildID)
	cacheKey := s.agenticCacheKey(jobID, run.BuildID, tc.Name, tc.FailureMessage)
	return s.client.doAnalyzeAgentic(ctx, browser, s.agenticOpts, cacheKey, s.systemPrompt, userPrompt)
}

func (s *Service) setUnavailable(tc *models.TestCase, err error) {
	if tc.AISummary == nil {
		tc.AISummary = &models.AISummary{
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			Summary:     "AI analysis unavailable: " + err.Error(),
			IsTransient: false,
		}
	}
}

// curatorMode is the value stored in models.AIAnalysis.Mode for results from
// the single-shot curator pipeline. Both "" (legacy) and this string are
// accepted as "curator" by shouldReanalyze.
const curatorMode = "curator"

// desiredMode picks the pipeline to use for this failure. Returns AgenticMode
// when agentic is enabled AND (always-on OR the module opts in via
// AgenticPreferrer), else curatorMode. Honors the run-scoped tools-unsupported
// flag so a 400 on one failure doesn't keep retrying for the rest of the run.
func (s *Service) desiredMode(run *models.BuildResult, tc *models.TestCase) string {
	if !s.agenticOn || s.toolsUnsupported.Load() {
		return curatorMode
	}
	if s.agenticAll {
		return AgenticMode
	}
	if p, ok := s.module.(AgenticPreferrer); ok {
		prefer, reason := p.PrefersAgentic(run, tc)
		if prefer {
			if reason == "" {
				reason = "module preference"
			}
			log.Printf("  ↻ %s opted into agentic mode: %s", tc.Name, reason)
			return AgenticMode
		}
	}
	return curatorMode
}

// shouldReanalyze returns true when a cached analysis must be discarded
// because the mode changed (e.g. agentic flipped on). Treats legacy unset Mode
// as "curator" so existing CAPZ caches keep hitting on curator runs.
func (s *Service) shouldReanalyze(tc *models.TestCase, desiredMode string) bool {
	cached := tc.AIAnalysis.Mode
	if cached == "" {
		cached = curatorMode
	}
	return cached != desiredMode
}

// cacheKey returns the cache key for the combined analysis call. For the
// "capi" module we keep the legacy "comprehensive:<hash>" shape so existing
// CAPZ deep-analysis caches stay valid (the on-disk payload shape changed,
// but stale entries are tolerated by the doAnalyze unmarshal fallback). New
// modules get an "analyze:<module>:<hash>" key.
func (s *Service) cacheKey(testName, failureMessage string) string {
	hash := failureHash(testName, failureMessage)
	if s.module.Name() == "capi" {
		return fmt.Sprintf("comprehensive:%x", hash)
	}
	return fmt.Sprintf("analyze:%s:%x", s.module.Name(), hash)
}

// agenticCacheKey scopes agentic results by job+build because the model's
// answer cites build-specific artifact paths and line numbers. Sharing the
// curator key would mean different builds of the same test serve each other
// the wrong line numbers.
func (s *Service) agenticCacheKey(jobID, buildID, testName, failureMessage string) string {
	hash := failureHash(testName, failureMessage)
	return fmt.Sprintf("agentic:%s:%s:%s:%x", s.module.Name(), jobID, buildID, hash)
}

// consecutiveKey scopes consecutive-failure counts by JobID + test name so
// same-named tests in different jobs do not share streaks.
func consecutiveKey(jobID, testName string) string {
	return jobID + "::" + testName
}

// failureHash builds the deterministic hash used by both cache key flavors.
// Matches the original cacheKey() / comprehensiveCacheKey() output byte-for-byte
// so legacy CAPZ caches keep hitting.
func failureHash(testName, failureMessage string) []byte {
	normalized := normalizeError(failureMessage)
	h := sha256.Sum256([]byte(testName + normalized))
	return h[:8]
}
