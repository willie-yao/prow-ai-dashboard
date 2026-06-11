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

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
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

	// agenticOpts is the resolved agentic config. When agenticOn is false
	// (the default), Analyze never goes agentic regardless of any other
	// signal.
	agenticOpts AgenticOptions
	agenticOn   bool
	agenticAll  bool // when true, every failure goes agentic; else module decides

	// universalOn selects the use_universal_path flow: agentic is always
	// on, no curator fallback on ErrToolsUnsupported, and Mode is stamped
	// UniversalMode so cached entries from any other mode get re-analyzed.
	universalOn bool

	// browserFactory provides per-build Browser instances. Required when
	// agentic mode is on.
	browserFactory artifacts.Factory

	// registry + enabledTools define which tools the agentic loop can call.
	registry     *tools.Registry
	enabledTools []string

	// skillSet is the loaded recipe set (project-scoped). nil when no
	// recipes are loaded or skills are disabled.
	skillSet *skills.Set

	// toolCaches memoizes a *tools.Cache per buildPrefix so all failures
	// of one build share expensive tier-2 discovery results.
	toolCaches sync.Map // map[string]*tools.Cache

	// toolsUnsupported is set after the first agentic call that returns
	// ErrToolsUnsupported, so subsequent failures fall straight back to
	// the curator pipeline.
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

// EnableAgentic switches the Service into mixed curator + agentic mode.
// always=true forces every failure through agentic; always=false defers to
// the Module's AgenticPreferrer. universalPath=true selects the agentic-only
// flow with no curator fallback on ErrToolsUnsupported.
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

// SetSkills installs the consumer's loaded recipe set. Safe to call once
// during fetcher startup, after EnableAgentic. The agentic loop honors the
// set only when critique is enabled (recipes feed the critique gate).
func (s *Service) SetSkills(set *skills.Set) {
	s.skillSet = set
}

// Analyze fills tc.AISummary and tc.AIAnalysis for a single failed test case.
// Skips if already analyzed under the current mode; short-circuits on known
// transients; runs agentic when enabled and applicable, else curator. On API
// failure leaves an "AI analysis unavailable" summary.
func (s *Service) Analyze(ctx context.Context, httpClient *http.Client, jobID, buildPrefix string, run *models.BuildResult, tc *models.TestCase) {
	desiredMode := s.desiredMode(run, tc)

	if tc.AISummary != nil && tc.AIAnalysis != nil && !s.shouldReanalyze(tc, desiredMode) {
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
		// Universal mode has no curator fallback; bail fast if tools are
		// known-unsupported from a prior failure in this run.
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

// runAgentic does the per-failure agentic call setup. Kept separate so
// Analyze stays readable.
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
		Skills:       s.skillSet,
	}
	return s.client.doAnalyzeAgentic(ctx, in, cacheKey, s.systemPrompt, userPrompt)
}

// modeForRun returns UniversalMode under use_universal_path, else AgenticMode.
func (s *Service) modeForRun() string {
	if s.universalOn {
		return UniversalMode
	}
	return AgenticMode
}

// toolCacheFor returns the *tools.Cache scoped to one build, creating it
// lazily on first use. Caches live for the Service lifetime (one fetcher run).
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

// curatorMode is the value stored in models.AIAnalysis.Mode for results
// from the single-shot curator pipeline. shouldReanalyze treats "" as this.
const curatorMode = "curator"

// desiredMode picks the pipeline to use for this failure. UniversalMode when
// use_universal_path is on; AgenticMode when agentic is enabled AND (always
// or the module opts in); else curatorMode. Honors the run-scoped
// tools-unsupported flag so a 400 on one failure doesn't keep retrying.
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
// because the mode changed or, for agentic-mode caches, because the prior
// analysis fell below the project's current floors or critique contract.
func (s *Service) shouldReanalyze(tc *models.TestCase, desiredMode string) bool {
	if tc.AIAnalysis.Mode != desiredMode {
		return true
	}
	return s.belowCurrentAgenticFloor(tc, desiredMode)
}

// belowCurrentAgenticFloor returns true when desiredMode is agentic AND the
// cached analysis fails any current quality gate: tool-call or GCS-byte
// floor, critique-not-passed when critique is on, critique version older
// than the engine's current version, or skill-set hash mismatch.
func (s *Service) belowCurrentAgenticFloor(tc *models.TestCase, desiredMode string) bool {
	if desiredMode != AgenticMode && desiredMode != UniversalMode {
		return false
	}
	if tc.AIAnalysis.ToolCalls < s.agenticOpts.MinToolCalls {
		return true
	}
	if tc.AIAnalysis.GCSBytes < s.agenticOpts.MinGCSBytes {
		return true
	}
	if s.agenticOpts.CritiqueEnabled {
		if !tc.AIAnalysis.CritiquePassed {
			return true
		}
		if tc.AIAnalysis.CritiqueVersion < currentCritiqueVersion {
			return true
		}
	}
	// Invalidate entries whose SkillSetHash doesn't match the currently-
	// loaded set so consumer recipe edits trigger re-analysis. Skills feed
	// the critique gate, so the hash is part of the contract exactly when
	// critique is on. Empty wantHash matches an entry stamped with no
	// recipes (intentional: nothing to invalidate if no recipes are loaded).
	if s.agenticOpts.CritiqueEnabled {
		wantHash := ""
		if s.skillSet != nil {
			wantHash = s.skillSet.Hash()
		}
		if tc.AIAnalysis.SkillSetHash != wantHash {
			return true
		}
	}
	return false
}

// cacheKey returns the cache key for the curator analysis call.
func (s *Service) cacheKey(testName, failureMessage string) string {
	return fmt.Sprintf("analyze:%s:%x", s.module.Name(), failureHash(testName, failureMessage))
}

// agenticCacheKey scopes agentic results by job+build because the model's
// answer cites build-specific artifact paths and line numbers.
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
func failureHash(testName, failureMessage string) []byte {
	normalized := normalizeError(failureMessage)
	h := sha256.Sum256([]byte(testName + normalized))
	return h[:8]
}
