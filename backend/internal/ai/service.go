package ai

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
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

	// skillSet is the loaded project recipe set. nil when no recipes are loaded.
	skillSet *skills.Set

	// toolCaches memoizes a *tools.Cache per buildPrefix so all failures
	// of one build share expensive tier-2 discovery results.
	toolCaches sync.Map // map[string]*tools.Cache

	// toolsUnsupported is set after the first agentic call that returns
	// ErrToolsUnsupported, so subsequent failures in the run skip straight
	// to "unavailable" instead of re-hitting an endpoint that can't do
	// function-calling.
	toolsUnsupported atomic.Bool

	// sourceRepoOwner/Name identify the project's own GitHub repo for resolving
	// repo-relative file citations. Empty until SetSourceRepo.
	sourceRepoOwner string
	sourceRepoName  string

	// linkVerifyCache memoizes GitHub file-existence checks across all
	// analyses in a run, keyed by "owner/repo/path" to existence.
	linkVerifyCache sync.Map

	// triageClient and triageOpts configure the optional cheap triage tier run
	// before the deep analysis. nil triageClient disables the cascade.
	triageClient *Client
	triageOpts   AgenticOptions

	// triageUnsupported latches once the triage endpoint rejects tools, so the
	// rest of the run skips straight to the deep tier instead of re-hitting it.
	triageUnsupported atomic.Bool

	// Cascade counters, updated atomically across concurrent analyses and read
	// once per run for a summary log: failures the triage tier resolved vs
	// escalated to the deep tier.
	triageResolved  atomic.Int64
	triageEscalated atomic.Int64
}

// NewService constructs a Service. systemPrompt is the full composed prompt and
// must be non-empty. consecutiveMap is keyed by consecutiveKey and may be nil.
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

// EnableAgentic installs the agentic loop's runtime dependencies: resolved
// options, per-build browser factory, tool registry, and enabled tool set.
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
// set only when critique is enabled, because recipes feed the critique gate.
func (s *Service) SetSkills(set *skills.Set) {
	s.skillSet = set
}

// SetSourceRepo records branding.source_repo for resolving repo-relative file
// citations. Safe to call once at fetcher startup.
func (s *Service) SetSourceRepo(owner, name string) {
	s.sourceRepoOwner = owner
	s.sourceRepoName = name
}

// EnableTriage installs the optional cheap triage tier (the model cascade). When
// set, Analyze investigates each failure on the triage client first and only
// escalates real bugs and ungrounded or budget-exhausted results to the deep
// tier. opts should carry a lighter iteration budget but keep the grounding
// floors so the cheap model must read the real logs before ruling a failure
// transient. Call once at fetcher startup, after EnableAgentic.
func (s *Service) EnableTriage(client *Client, opts AgenticOptions) {
	s.triageClient = client
	s.triageOpts = opts
}

// TriageStats returns how many failures the triage tier resolved on the cheap
// model versus escalated to the deep tier this run. Both are zero when the
// cascade is off.
func (s *Service) TriageStats() (resolved, escalated int) {
	return int(s.triageResolved.Load()), int(s.triageEscalated.Load())
}

// Analyze fills tc.AISummary and tc.AIAnalysis for a single failed test case
// using the agentic tool-calling loop. Skips if already analyzed and still
// meets the current quality floors. On API failure or endpoints without
// function-calling, it leaves an "AI analysis unavailable" summary.
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

	// Surface endpoints without function-calling as unavailable. There is no
	// tools-free analysis path to degrade to.
	if s.toolsUnsupported.Load() {
		s.setUnavailable(tc, fmt.Errorf("AI endpoint requires function-calling support"))
		return
	}
	summary, analysis, err := s.analyzeCascade(ctx, jobID, buildPrefix, run, tc, userPrompt)
	if err != nil {
		if errors.Is(err, ErrToolsUnsupported) {
			s.toolsUnsupported.Store(true)
			log.Printf("  ⚠ AI endpoint rejected tools; analysis unavailable: %v", err)
			s.setUnavailable(tc, fmt.Errorf("AI endpoint requires function-calling support: %w", err))
			return
		}
		log.Printf("  ⚠ Agentic AI analysis failed for %s: %v", tc.Name, err)
		s.setUnavailable(tc, err)
		return
	}
	tc.AISummary = summary
	tc.AIAnalysis = analysis
	if analysis != nil {
		analysis.FileLinks = s.resolveFileLinks(ctx, httpClient, tc)
	}
}

// analyzeCascade runs the model cascade: when a triage tier is configured, it
// investigates on the cheap model first and short-circuits a grounded transient,
// otherwise it escalates to the deep tier. With no triage tier it is a single
// deep-tier call. A triage infra error (including an endpoint that rejects
// tools) escalates rather than dropping the analysis.
func (s *Service) analyzeCascade(ctx context.Context, jobID, buildPrefix string, run *models.BuildResult, tc *models.TestCase, userPrompt string) (*models.AISummary, *models.AIAnalysis, error) {
	if s.triageClient != nil && !s.triageUnsupported.Load() {
		summary, analysis, err := s.runAgenticTier(ctx, s.triageClient, s.triageOpts, ":triage", jobID, buildPrefix, run, tc, userPrompt)
		if err == nil {
			if s.triageAccepts(summary, analysis) {
				if analysis != nil {
					analysis.Tier = "triage"
				}
				s.triageResolved.Add(1)
				return summary, analysis, nil
			}
		} else {
			if errors.Is(err, ErrToolsUnsupported) {
				s.triageUnsupported.Store(true)
			}
			log.Printf("  ⚠ triage tier unavailable for %s, escalating to deep: %v", tc.Name, err)
		}
	}

	summary, analysis, err := s.runAgenticTier(ctx, s.client, s.agenticOpts, "", jobID, buildPrefix, run, tc, userPrompt)
	if err != nil {
		return nil, nil, err
	}
	if s.triageClient != nil {
		if analysis != nil {
			analysis.Tier = "deep"
			analysis.Escalated = true
		}
		s.triageEscalated.Add(1)
	}
	return summary, analysis, nil
}

// triageAccepts reports whether the triage tier produced a grounded transient
// verdict, the only outcome that short-circuits the deep tier. A real bug, a
// transient below the triage grounding floors, or a budget-exhausted run all
// escalate, so the cheap tier never finalizes a real bug.
func (s *Service) triageAccepts(summary *models.AISummary, analysis *models.AIAnalysis) bool {
	if summary == nil || analysis == nil {
		return false
	}
	if !summary.IsTransient || analysis.BudgetExhausted {
		return false
	}
	if analysis.ToolCalls < s.triageOpts.MinToolCalls || analysis.GCSBytes < s.triageOpts.MinGCSBytes {
		return false
	}
	return true
}

// runAgenticTier does the per-failure agentic call setup for one cascade tier.
// keySuffix namespaces the tier's AI-result cache entries so triage and deep
// never collide in the shared cache; the deep tier uses no suffix to preserve
// single-tier cache entries. The per-build tool cache is shared across tiers, so
// the deep tier reuses any artifacts the triage tier already fetched.
func (s *Service) runAgenticTier(ctx context.Context, client *Client, opts AgenticOptions, keySuffix, jobID, buildPrefix string, run *models.BuildResult, tc *models.TestCase, userPrompt string) (*models.AISummary, *models.AIAnalysis, error) {
	if s.browserFactory == nil {
		return nil, nil, fmt.Errorf("agentic mode enabled but no browser factory configured")
	}
	if s.registry == nil {
		return nil, nil, fmt.Errorf("agentic mode enabled but no tool registry configured")
	}
	browser := s.browserFactory.ForBuild(buildPrefix, run.JobName+"/"+run.BuildID)
	cache := s.toolCacheFor(buildPrefix)
	cacheKey := s.agenticCacheKey(jobID, run.BuildID, tc.Name, tc.FailureMessage) + keySuffix
	in := AgenticInputs{
		Browser:      browser,
		Opts:         opts,
		Registry:     s.registry,
		EnabledTools: s.enabledTools,
		Cache:        cache,
		WebURLBase:   run.WebURL,
		Mode:         AgenticMode,
		Skills:       s.skillSet,
	}
	return client.doAnalyzeAgentic(ctx, in, cacheKey, s.systemPrompt, userPrompt)
}

// toolCacheFor returns the *tools.Cache scoped to one build, creating it
// lazily on first use. Caches live for one fetcher run.
func (s *Service) toolCacheFor(buildPrefix string) *tools.Cache {
	if existing, ok := s.toolCaches.Load(buildPrefix); ok {
		return existing.(*tools.Cache)
	}
	fresh := tools.NewCache()
	actual, _ := s.toolCaches.LoadOrStore(buildPrefix, fresh)
	return actual.(*tools.Cache)
}

func (s *Service) setUnavailable(tc *models.TestCase, err error) {
	// Overwrite only an engine-written "unavailable" placeholder with no model
	// analysis attached. Errored failures are re-analyzed on every run, so stale
	// endpoint outage or misconfiguration errors must not persist after the
	// cause changes. Real summaries and transient classifications are preserved.
	if tc.AISummary != nil && (tc.AIAnalysis != nil || !isUnavailableSummary(tc.AISummary)) {
		return
	}
	tc.AISummary = &models.AISummary{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Summary:     unavailablePrefix + err.Error(),
		IsTransient: false,
	}
}

// unavailablePrefix marks a summary the engine wrote because analysis could
// not complete and no model result exists.
const unavailablePrefix = "AI analysis unavailable: "

// isUnavailableSummary reports whether a later run should replace an
// engine-written "unavailable" placeholder.
func isUnavailableSummary(s *models.AISummary) bool {
	return s != nil && !s.IsTransient && strings.HasPrefix(s.Summary, unavailablePrefix)
}

// shouldReanalyze returns true when a cached analysis must be discarded
// because it predates the single agentic path or fails any current quality gate.
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
	// A triage-resolved result never passed through the deep floors, so revalidate
	// it against the triage tier's own floors (and the prompt). With the cascade
	// now disabled, force a deep re-analysis instead of trusting a stale cheap
	// verdict.
	if tc.AIAnalysis.Tier == "triage" {
		if s.triageClient == nil {
			return true
		}
		if tc.AIAnalysis.ToolCalls < s.triageOpts.MinToolCalls || tc.AIAnalysis.GCSBytes < s.triageOpts.MinGCSBytes {
			return true
		}
		return tc.AIAnalysis.PromptHash != PromptFingerprint(s.systemPrompt)
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
	// Invalidate entries whose SkillSetHash doesn't match the loaded set
	// so consumer recipe edits trigger re-analysis. Skills feed
	// the critique gate, so the hash is part of the contract exactly when
	// critique is on. Empty wantHash matches an entry stamped with no
	// recipes. Nothing is invalidated when no recipes are loaded.
	if s.agenticOpts.CritiqueEnabled {
		wantHash := ""
		if s.skillSet != nil {
			wantHash = s.skillSet.Hash()
		}
		if tc.AIAnalysis.SkillSetHash != wantHash {
			return true
		}
	}
	// The prompt is always sent to the model, so prompt edits invalidate the
	// entry without a critique dependency.
	if tc.AIAnalysis.PromptHash != PromptFingerprint(s.systemPrompt) {
		return true
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
