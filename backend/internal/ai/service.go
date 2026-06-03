package ai

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
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

	// universalOn flags the use_universal_path flow: agentic is always on,
	// no curator fallback on ErrToolsUnsupported (the user explicitly opted
	// into an agentic-only path), and AIAnalysis.Mode is stamped as
	// UniversalMode so cached entries from any other mode get re-analyzed.
	universalOn bool

	// browserFactory provides per-build Browser instances. nil when
	// agentic mode is off, in which case Analyze ignores it.
	browserFactory artifacts.Factory

	// registry + enabledTools define which tools the agentic loop can call.
	// Constructed once per Service at EnableAgentic time so per-project tool
	// configuration is honored without rebuilding on every failure.
	registry     *tools.Registry
	enabledTools []string

	// toolCaches memoizes a *tools.Cache per buildPrefix. All failures of one
	// build share the same cache so expensive tier-2 discovery (cluster
	// listings, controller-log enumerations, etc.) is paid once per build
	// rather than once per failure.
	toolCaches sync.Map // map[string]*tools.Cache

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
// Browsers. registry + enabledTools define the tool surface exposed to the
// model (e.g. filesystem + k8s tier-2). always=true forces every failure
// through agentic mode; always=false defers the choice to the Module's
// AgenticPreferrer (modules that don't implement it stay on curator).
// universalPath=true selects the use_universal_path flow: implies always-on
// agentic AND disables the ErrToolsUnsupported curator fallback (the
// universal flow is explicitly an agentic-only pipeline).
//
// Safe to call once at Service construction; not safe for concurrent use.
func (s *Service) EnableAgentic(opts AgenticOptions, factory artifacts.Factory, registry *tools.Registry, enabledTools []string, always, universalPath bool) {
	s.agenticOn = true
	s.agenticAll = always || universalPath
	s.universalOn = universalPath
	s.agenticOpts = opts
	s.browserFactory = factory
	s.registry = registry
	s.enabledTools = enabledTools
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

	if desiredMode == AgenticMode || desiredMode == UniversalMode {
		// Universal mode bails fast if a prior failure already exposed that
		// the endpoint can't do tools. No curator fallback: the user
		// explicitly opted into an agentic-only path.
		if s.universalOn && s.toolsUnsupported.Load() {
			s.setUnavailable(tc, fmt.Errorf("universal AI path requires endpoint with function-calling support"))
			return
		}
		summary, analysis, err := s.runAgentic(ctx, jobID, buildPrefix, run, tc, userPrompt)
		if err == nil {
			tc.AISummary = summary
			tc.AIAnalysis = analysis
			return
		}
		if errors.Is(err, ErrToolsUnsupported) {
			s.toolsUnsupported.Store(true)
			if s.universalOn {
				log.Printf("  ⚠ AI endpoint rejected tools; universal mode has no curator fallback: %v", err)
				s.setUnavailable(tc, fmt.Errorf("universal AI path requires endpoint with function-calling support: %w", err))
				return
			}
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
// build-scoped tool cache + call). Kept separate so Analyze stays readable.
func (s *Service) runAgentic(ctx context.Context, jobID, buildPrefix string, run *models.BuildResult, tc *models.TestCase, userPrompt string) (*models.AISummary, *models.AIAnalysis, error) {
	if s.browserFactory == nil {
		return nil, nil, fmt.Errorf("agentic mode enabled but no browser factory configured")
	}
	if s.registry == nil {
		return nil, nil, fmt.Errorf("agentic mode enabled but no tool registry configured")
	}
	browser := s.browserFactory.ForBuild(buildPrefix, run.JobName+"/"+run.BuildID)
	cache := s.toolCacheFor(buildPrefix)
	cacheKey := s.agenticCacheKey(jobID, run.BuildID, tc.Name, tc.FailureMessage)
	in := AgenticInputs{
		Browser:      browser,
		Opts:         s.agenticOpts,
		Registry:     s.registry,
		EnabledTools: s.enabledTools,
		Cache:        cache,
		WebURLBase:   run.WebURL,
		Mode:         s.modeForRun(),
	}
	return s.client.doAnalyzeAgentic(ctx, in, cacheKey, s.systemPrompt, userPrompt)
}

// modeForRun returns the Mode value to stamp on results produced by this
// Service's agentic path. UniversalMode under use_universal_path, AgenticMode
// otherwise.
func (s *Service) modeForRun() string {
	if s.universalOn {
		return UniversalMode
	}
	return AgenticMode
}

// toolCacheFor returns the *tools.Cache scoped to one build. Cache instances
// are created lazily on first use and live for the lifetime of the Service,
// which is bounded to one fetcher run.
func (s *Service) toolCacheFor(buildPrefix string) *tools.Cache {
	if existing, ok := s.toolCaches.Load(buildPrefix); ok {
		return existing.(*tools.Cache)
	}
	fresh := tools.NewCache()
	actual, _ := s.toolCaches.LoadOrStore(buildPrefix, fresh)
	return actual.(*tools.Cache)
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

// desiredMode picks the pipeline to use for this failure. Returns
// UniversalMode when use_universal_path is on (sticky regardless of the
// tools-unsupported flag — Analyze handles the early bail), AgenticMode
// when regular agentic is enabled AND (always-on OR the module opts in via
// AgenticPreferrer), else curatorMode. Honors the run-scoped
// tools-unsupported flag so a 400 on one failure doesn't keep retrying for
// the rest of the run.
func (s *Service) desiredMode(run *models.BuildResult, tc *models.TestCase) string {
	if s.universalOn {
		return UniversalMode
	}
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
// because the mode changed (e.g. agentic flipped on) or, for agentic-mode
// caches, because the prior analysis fell below the project's current
// MinToolCalls floor. Treats legacy unset Mode as "curator" so existing
// CAPZ caches keep hitting on curator runs.
func (s *Service) shouldReanalyze(tc *models.TestCase, desiredMode string) bool {
	cached := tc.AIAnalysis.Mode
	if cached == "" {
		cached = curatorMode
	}
	if cached != desiredMode {
		return true
	}
	return s.belowCurrentAgenticFloor(tc, desiredMode)
}

// belowCurrentAgenticFloor returns true when desiredMode is one of the
// agentic modes AND the cached analysis recorded fewer tool calls than the
// project's current MinToolCalls floor. Used to invalidate pre-floor cache
// entries (and below-floor accepted entries) so a freshly-raised floor
// actually re-runs the loop. Returns false for the curator path because
// curator analyses legitimately have ToolCalls=0 by design.
func (s *Service) belowCurrentAgenticFloor(tc *models.TestCase, desiredMode string) bool {
	if desiredMode != AgenticMode && desiredMode != UniversalMode {
		return false
	}
	return tc.AIAnalysis.ToolCalls < s.agenticOpts.MinToolCalls
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
