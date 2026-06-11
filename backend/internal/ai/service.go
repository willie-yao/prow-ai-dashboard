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
// API Client with the universal prompt builder, the composed system prompt, and
// a snapshot of consecutive failure counts. Every failure is analyzed by the
// agentic tool-calling loop; there is no other path.
type Service struct {
	client         *Client
	module         Module
	systemPrompt   string
	consecutiveMap map[string]int

	// agenticOpts is the resolved agentic config.
	agenticOpts AgenticOptions

	// browserFactory provides per-build Browser instances.
	browserFactory artifacts.Factory

	// registry + enabledTools define which tools the agentic loop can call.
	registry     *tools.Registry
	enabledTools []string

	// skillSet is the loaded recipe set (project-scoped). nil when no
	// recipes are loaded.
	skillSet *skills.Set

	// toolCaches memoizes a *tools.Cache per buildPrefix so all failures
	// of one build share expensive tier-2 discovery results.
	toolCaches sync.Map // map[string]*tools.Cache

	// toolsUnsupported is set after the first agentic call that returns
	// ErrToolsUnsupported, so subsequent failures in the run skip straight
	// to "unavailable" instead of re-hitting an endpoint that can't do
	// function-calling.
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

// EnableAgentic installs the agentic loop's runtime dependencies (resolved
// options, per-build browser factory, tool registry, and enabled tool set).
// Must be called once at fetcher startup before Analyze.
//
// Safe to call once at Service construction; not safe for concurrent use.
func (s *Service) EnableAgentic(opts AgenticOptions, factory artifacts.Factory, registry *tools.Registry, enabledTools []string) {
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

// Analyze fills tc.AISummary and tc.AIAnalysis for a single failed test case
// using the agentic tool-calling loop. Skips if already analyzed and still
// meets the current quality floors. On API failure (or an endpoint without
// function-calling) it leaves an "AI analysis unavailable" summary.
func (s *Service) Analyze(ctx context.Context, httpClient *http.Client, jobID, buildPrefix string, run *models.BuildResult, tc *models.TestCase) {
	if tc.AISummary != nil && tc.AIAnalysis != nil && !s.shouldReanalyze(tc) {
		return
	}

	log.Printf("  🔍 Analyzing: %s [%s]", tc.Name, AgenticMode)
	consec := s.consecutiveMap[consecutiveKey(jobID, tc.Name)]
	if consec < 1 {
		consec = 1
	}

	userPrompt := s.module.AnalysisPrompt(ctx, httpClient, run, tc, consec)

	// No curator fallback: an endpoint that can't do function-calling
	// (detected on an earlier failure this run, or on this call) surfaces
	// as unavailable rather than degrading to a tools-free prompt.
	if s.toolsUnsupported.Load() {
		s.setUnavailable(tc, fmt.Errorf("AI endpoint requires function-calling support"))
		return
	}
	summary, analysis, err := s.runAgentic(ctx, jobID, buildPrefix, run, tc, userPrompt)
	if err != nil {
		if errors.Is(err, ErrToolsUnsupported) {
			s.toolsUnsupported.Store(true)
			log.Printf("  ⚠ AI endpoint rejected tools; analysis unavailable (no curator fallback): %v", err)
			s.setUnavailable(tc, fmt.Errorf("AI endpoint requires function-calling support: %w", err))
			return
		}
		log.Printf("  ⚠ Agentic AI analysis failed for %s: %v", tc.Name, err)
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
		Mode:         AgenticMode,
		Skills:       s.skillSet,
	}
	return s.client.doAnalyzeAgentic(ctx, in, cacheKey, s.systemPrompt, userPrompt)
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

// shouldReanalyze returns true when a cached analysis must be discarded
// because the mode changed or, for agentic-mode caches, because the prior
// shouldReanalyze returns true when a cached analysis must be discarded
// because it predates the single agentic path (stale mode) or fails any
// current quality gate.
func (s *Service) shouldReanalyze(tc *models.TestCase) bool {
	if tc.AIAnalysis.Mode != AgenticMode {
		return true
	}
	return s.belowCurrentAgenticFloor(tc)
}

// belowCurrentAgenticFloor returns true when the cached analysis fails any
// current quality gate: tool-call or GCS-byte floor, critique-not-passed when
// critique is on, critique version older than the engine's current version, or
// skill-set hash mismatch.
func (s *Service) belowCurrentAgenticFloor(tc *models.TestCase) bool {
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
