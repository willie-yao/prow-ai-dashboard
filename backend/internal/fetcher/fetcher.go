// Package fetcher contains the orchestration that cmd/fetcher invokes:
// loading project config, discovering jobs, fetching builds (with caching),
// running AI failure analysis, and writing dashboard output. The cmd binary
// is just flag parsing plus registry wiring; everything else lives here.
package fetcher

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aggregator"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/modules/universal"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/filesystem"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/k8s"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/collectors"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/junit"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/notify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

// Options is the parsed, validated invocation for a single fetcher run.
// Callers (cmd/fetcher) construct it from flags and wire the built-in
// collector and AI module factories before calling Run.
type Options struct {
	ProjectDir   string
	OutDir       string
	BuildsPerJob int
	Workers      int
	Timeout      time.Duration
	// IncludePresubmits, when true, fetches presubmit jobs in addition
	// to periodics. ORed with cfg.Source.IncludePresubmits: either source
	// turning it on enables presubmits. The workflow input cannot
	// override the consumer's yaml choice to OFF.
	IncludePresubmits bool
	EnableAI          bool
	Collectors        *CollectorRegistry
	// Version is the engine's own version string (e.g. "v1.2.0"), embedded at
	// build time. Logged at startup and compared against the config's
	// min_engine_version. Empty/"dev" disables the comparison.
	Version string
}

// Run executes the full pipeline: load → discover → fetch → aggregate →
// optionally analyze with AI → write output → optionally notify. Per-job
// fetch errors are logged but do not abort the run; only startup failures
// (config load, collector wiring) return an error.
func Run(ctx context.Context, opts Options) error {
	if opts.Collectors == nil {
		return fmt.Errorf("fetcher.Options.Collectors registry is required")
	}

	cfg, err := project.Load(filepath.Join(opts.ProjectDir, "project.yaml"))
	if err != nil {
		return fmt.Errorf("loading project config: %w", err)
	}
	log.Printf("Project: %s (%s) storage=%s bucket=%s",
		cfg.Name, cfg.DisplayShortName(), cfg.StorageConfig().Provider, cfg.Storage.Bucket)
	if opts.Version != "" {
		log.Printf("Engine version: %s", opts.Version)
	}
	if w := cfg.EngineVersionWarning(opts.Version); w != "" {
		log.Printf("⚠ %s", w)
	}

	// Validate the collector reference against the registry up front so a
	// typo'd artifacts.collector fails before any expensive work.
	if !opts.Collectors.Has(cfg.CollectorName()) {
		return fmt.Errorf("unknown artifacts.collector %q (registered: %s)",
			cfg.CollectorName(), strings.Join(opts.Collectors.Names(), ", "))
	}

	var aiSystemPrompt string
	var aiSkillSet *skills.Set
	if opts.EnableAI {
		_, prompt, err := project.LoadDir(opts.ProjectDir)
		if err != nil {
			return fmt.Errorf("loading AI prompt: %w", err)
		}
		aiSystemPrompt = ai.ComposeSystemPrompt(prompt)

		// Load consumer-owned recipes from <project_dir>/skills/*.yaml.
		// A missing directory returns an empty Set (recipes are opt-in).
		// Parse or regex compile errors are hard startup errors.
		set, err := skills.Load(opts.ProjectDir)
		if err != nil {
			return fmt.Errorf("loading AI skills: %w", err)
		}
		aiSkillSet = set
		if n := len(aiSkillSet.Skills()); n > 0 {
			log.Printf("Loaded %d AI skill recipe(s) from %s/skills/ (hash=%s)",
				n, opts.ProjectDir, shortHash(aiSkillSet.Hash()))
		}
	}

	aiToken := os.Getenv("AI_TOKEN")
	if opts.EnableAI && aiToken == "" {
		aiToken = os.Getenv("GITHUB_TOKEN")
	}
	if opts.EnableAI && aiToken == "" {
		log.Println("Warning: -ai enabled but no AI_TOKEN or GITHUB_TOKEN set, disabling AI analysis")
		opts.EnableAI = false
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	client := &http.Client{Timeout: 30 * time.Second}
	backend, err := storage.New(cfg.StorageConfig(), client)
	if err != nil {
		return fmt.Errorf("configuring storage: %w", err)
	}

	collector, err := opts.Collectors.Build(cfg, backend, client)
	if err != nil {
		return fmt.Errorf("building collector: %w", err)
	}
	log.Printf("Using artifact collector: %s", collector.Name())

	// Step 1: Discover jobs, either from test-infra (testgrid) or by listing
	// the artifact bucket's own job indexes.
	var jobs []models.ProwJob
	includePresubmits := opts.IncludePresubmits || cfg.Source.IncludePresubmits
	switch cfg.EffectiveDiscoverySource() {
	case project.DiscoveryBucket:
		log.Println("Discovering jobs from the storage bucket...")
		jobs, err = prowbuild.DiscoverJobs(ctx, backend, includePresubmits, cfg.Discovery.JobFilters)
		if err != nil {
			return fmt.Errorf("discovering jobs from bucket: %w", err)
		}
		// Bucket discovery has no job-config YAML, so assign categories from
		// the project rules here (the testgrid path does this at parse time).
		for i := range jobs {
			jobs[i].Category = cfg.Categorize(jobs[i].Name)
		}
	default:
		log.Println("Fetching job configs from test-infra...")
		jobs, err = jobconfig.FetchJobConfigs(ctx, client, cfg)
		if err != nil {
			return fmt.Errorf("fetching job configs: %w", err)
		}
		if !includePresubmits {
			var periodic []models.ProwJob
			for _, j := range jobs {
				if j.JobType == models.JobTypePeriodic {
					periodic = append(periodic, j)
				}
			}
			jobs = periodic
		}
	}
	log.Printf("Discovered %d jobs (presubmits=%v)", len(jobs), includePresubmits)

	// Derive the display-only short-name prefix from the discovered set so
	// the frontend can render compact job names without consumers having to
	// hand-maintain the prefix.
	cfg.ShortNamePrefix = jobconfig.DerivePeriodicPrefix(jobs)

	// Step 2: For each job, discover builds and fetch results. Cached data
	// is reused for completed builds; only PENDING runs are re-fetched.
	cachedJobs := loadCachedJobDetails(opts.OutDir)

	type jobResult struct {
		job  models.ProwJob
		runs []models.BuildResult
	}

	results := make([]jobResult, len(jobs))
	sem := make(chan struct{}, opts.Workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var fetchErrors []error

	for i, job := range jobs {
		wg.Add(1)
		go func(idx int, j models.ProwJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			runs, err := fetchJobRunsCached(ctx, backend, cfg, collector, &j, opts.BuildsPerJob, cachedJobs[j.JobID])
			if err != nil {
				mu.Lock()
				fetchErrors = append(fetchErrors, fmt.Errorf("job %s: %w", j.Name, err))
				mu.Unlock()
				log.Printf("  ⚠ %s: %v", j.Name, err)
				return
			}

			results[idx] = jobResult{job: j, runs: runs}
			passed := 0
			for _, r := range runs {
				if r.Passed {
					passed++
				}
			}
			log.Printf("  ✓ %s: %d runs (%d passed)", j.Name, len(runs), passed)
		}(i, job)
	}
	wg.Wait()

	if len(fetchErrors) > 0 {
		log.Printf("Warning: %d jobs had fetch errors", len(fetchErrors))
	}

	// Step 3: Aggregate.
	now := time.Now().UTC()
	dashboard := models.Dashboard{GeneratedAt: now}
	var details []models.JobDetail

	for _, r := range results {
		if r.job.Name == "" {
			continue // skipped due to fetch error
		}
		dashboard.Jobs = append(dashboard.Jobs, aggregator.ComputeJobSummary(r.job, r.runs))
		details = append(details, models.JobDetail{
			Name:    r.job.Name,
			JobID:   r.job.JobID,
			JobType: r.job.JobType,
			Repo:    r.job.Repo,
			Runs:    r.runs,
		})
	}

	// Step 4: Flakiness report + search index. Maps keyed by JobID so
	// same-named jobs across repos do not overwrite each other.
	jobResultMap := make(map[string][]models.BuildResult, len(results))
	for _, r := range results {
		if r.job.Name == "" {
			continue
		}
		jobResultMap[r.job.JobID] = r.runs
	}
	flakinessReport := aggregator.ComputeFlakinessReport(jobResultMap, jobs, now)
	log.Printf("Flakiness report: %d most flaky, %d persistent, %d recently broken",
		len(flakinessReport.MostFlaky), len(flakinessReport.PersistentFailures), len(flakinessReport.RecentlyBroken))

	searchIndex := aggregator.BuildSearchIndex(jobResultMap, jobs, now)
	log.Printf("Search index: %d entries", len(searchIndex.Entries))

	// Step 5: AI failure analysis (optional).
	if opts.EnableAI {
		analyzeFailuresWithAI(ctx, cfg, details, flakinessReport, aiToken, opts.OutDir, aiSystemPrompt, aiSkillSet)
	}

	log.Printf("Writing output to %s/ (%d jobs)", opts.OutDir, len(dashboard.Jobs))
	if err := output.WriteAll(opts.OutDir, cfg, dashboard, details, flakinessReport, searchIndex); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}

	// Step 6: Slack/Teams notifications for persistent failures (optional).
	if slackWebhookURL := os.Getenv("SLACK_WEBHOOK_URL"); slackWebhookURL != "" {
		notifier := notify.NewNotifier(
			slackWebhookURL,
			filepath.Join(opts.OutDir, "notification_state.json"),
			cfg.Branding.SiteURL,
			backend.ProwURL("logs/"),
		)
		stats, err := notifier.ProcessFailures(ctx, flakinessReport, details)
		if err != nil {
			log.Printf("Warning: notification processing failed: %v", err)
		} else {
			log.Printf("📢 Notifications: %d new alerts, %d recoveries", stats.NewAlerts, stats.Recoveries)
		}
		if err := notifier.SaveState(); err != nil {
			log.Printf("Warning: failed to save notification state: %v", err)
		}
	} else {
		log.Println("Notifications: skipped (no SLACK_WEBHOOK_URL)")
	}

	log.Println("Done!")
	return nil
}

// loadCachedJobDetails loads existing per-job JSON files from the output
// dir and returns a map of JobID → cached BuildResults (keyed by build ID).
func loadCachedJobDetails(outDir string) map[string]map[string]models.BuildResult {
	cached := make(map[string]map[string]models.BuildResult)
	jobsDir := filepath.Join(outDir, "jobs")
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return cached
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(jobsDir, e.Name()))
		if err != nil {
			continue
		}
		var detail models.JobDetail
		if json.Unmarshal(data, &detail) != nil || detail.JobID == "" {
			continue
		}
		builds := make(map[string]models.BuildResult, len(detail.Runs))
		for _, r := range detail.Runs {
			// Only cache completed builds (not PENDING).
			if r.Result != "PENDING" && r.Result != "" {
				builds[r.BuildID] = r
			}
		}
		if len(builds) > 0 {
			cached[detail.JobID] = builds
		}
	}
	return cached
}

// fetchJobRunsCached discovers recent builds and reuses cached data for
// completed builds. Per-build fetch errors are logged but do not abort.
func fetchJobRunsCached(ctx context.Context, backend storage.Backend, cfg *project.Config, collector collectors.Collector, job *models.ProwJob, count int, cachedBuilds map[string]models.BuildResult) ([]models.BuildResult, error) {
	builds, err := prowbuild.ListRecentBuilds(ctx, backend, job, count)
	if err != nil {
		return nil, fmt.Errorf("listing builds: %w", err)
	}

	var runs []models.BuildResult
	fetched, reused := 0, 0
	for _, b := range builds {
		if cached, ok := cachedBuilds[b.ID]; ok {
			runs = append(runs, cached)
			reused++
			continue
		}
		result, err := fetchBuildResult(ctx, backend, collector, job, b)
		if err != nil {
			log.Printf("    ⚠ %s/%s: %v", job.Name, b.ID, err)
			continue
		}
		runs = append(runs, *result)
		fetched++
	}

	if reused > 0 {
		log.Printf("    💾 %s: %d cached, %d fetched", job.Name, reused, fetched)
	}

	return runs, nil
}

// fetchBuildResult fetches metadata and JUnit XML for a single build, then
// delegates per-test artifact discovery to the configured collector (a no-op
// for the default generic collector; the agentic loop fetches what it needs
// via tools).
func fetchBuildResult(ctx context.Context, backend storage.Backend, collector collectors.Collector, job *models.ProwJob, build prowbuild.Build) (*models.BuildResult, error) {
	loc := prowbuild.BuildLocation{
		JobLocation: prowbuild.JobLocation{JobType: job.JobType, Repo: job.Repo},
		JobName:     job.Name,
		BuildID:     build.ID,
		PullNumber:  build.PullNumber,
	}

	info, err := prowbuild.FetchBuildInfo(ctx, backend, loc)
	if err != nil {
		return nil, fmt.Errorf("fetching build info: %w", err)
	}

	result := &models.BuildResult{BuildInfo: *info, TestCases: []models.TestCase{}}

	junitPaths, err := prowbuild.DiscoverJUnitPaths(ctx, backend, loc)
	if err != nil {
		log.Printf("    ⚠ %s/%s: discovering junit files: %v", job.Name, build.ID, err)
		return result, nil
	}
	if len(junitPaths) == 0 {
		return result, nil
	}

	for _, junitPath := range junitPaths {
		result.JUnitURLs = append(result.JUnitURLs, backend.WebURL(junitPath))
		junitData, err := storage.ReadAll(ctx, backend, junitPath)
		if err != nil {
			log.Printf("    ⚠ %s/%s: fetching %s: %v", job.Name, build.ID, path.Base(junitPath), err)
			continue
		}
		testCases, err := junit.ParseFile(junitData, path.Base(junitPath))
		if err != nil {
			log.Printf("    ⚠ %s/%s: parsing %s: %v", job.Name, build.ID, path.Base(junitPath), err)
			continue
		}
		result.TestCases = append(result.TestCases, testCases...)
	}

	for _, tc := range result.TestCases {
		result.TestsTotal++
		switch tc.Status {
		case "passed":
			result.TestsPassed++
		case "failed":
			result.TestsFailed++
		case "skipped":
			result.TestsSkipped++
		}
	}

	if err := collector.CollectArtifacts(ctx, loc, result); err != nil {
		log.Printf("    ⚠ %s/%s: collector %s: %v", job.Name, build.ID, collector.Name(), err)
	}

	return result, nil
}

// Budget auto-sizing factors. The agentic loop needs a client-side byte
// budget to compact the conversation before it hits the endpoint's hard
// context limit (Dynamo/TRT-LLM 500s on overflow rather than degrading). The
// engine derives the budgets from the model's reported context window so they
// are not configurable per deployment. Budgets are in bytes; the window is in
// tokens.
const (
	avgBytesPerToken       = 4  // rough bytes/token for logs/JSON/English text
	modelBudgetWindowPct   = 50 // evidence-gathering cap ~= half the window
	contextBudgetWindowPct = 75 // compaction guard ~= 3/4 the window (response + estimate headroom)

	// fallbackModelByteBudget is used when the endpoint doesn't report a
	// context window (e.g. GitHub Copilot's /v1/models). Matches the
	// historical default. The compaction guard stays off (0) in that case,
	// which is safe for large-window models and the only prior behavior.
	fallbackModelByteBudget = 300_000

	// gcsByteBudget is the fixed aggregate ceiling on bytes fetched from GCS
	// across one analysis. Not configurable: it's a runaway-fetch safety cap,
	// not a tuning knob (per-call fetches are already capped at 64 MB, and
	// max_iters + timeout bound the loop). Rarely approached in practice
	// (~MBs per analysis).
	gcsByteBudget = 1_000_000_000
)

// analyzeFailuresWithAI runs the agentic AI analysis on every failed test
// case. The agentic tool-calling loop is the only analysis path.
func analyzeFailuresWithAI(ctx context.Context, cfg *project.Config, details []models.JobDetail, flakinessReport models.FlakinessReport, token, outDir, systemPrompt string, skillSet *skills.Set) {
	aiClient := ai.NewClientWithOptions(ai.Options{
		Token:        token,
		CacheDir:     outDir,
		Endpoint:     aiEndpoint(cfg),
		Model:        aiModel(cfg),
		ExtraHeaders: aiHeaders(cfg),
	})
	defer func() {
		if err := aiClient.Cache().Save(); err != nil {
			log.Printf("Warning: failed to save AI cache: %v", err)
		}
	}()

	consecutiveMap := make(map[string]int)
	for _, tf := range flakinessReport.PersistentFailures {
		consecutiveMap[tf.JobID+"::"+tf.TestName] = tf.ConsecutiveFailures
	}

	module := universal.New()
	service := ai.NewService(aiClient, module, systemPrompt, consecutiveMap)
	// Used to resolve and verify repo-relative file citations against the
	// project's own GitHub repo (branding.source_repo).
	service.SetSourceRepo(cfg.Branding.SourceRepo.Owner, cfg.Branding.SourceRepo.Name)

	eff := cfg.AI.EffectiveAgentic()
	// Recipe files feed the critique gate, so shipping them is the opt-in
	// for both the recipes and the gate they need: auto-enable critique when
	// recipes are present (an explicit critique block still supplies
	// max_retries via EffectiveAgentic).
	if !eff.Critique.Enabled && skillSet != nil && len(skillSet.Skills()) > 0 {
		eff.Critique.Enabled = true
		log.Printf("🧪 %d skill recipe(s) present; auto-enabling the critique gate they feed", len(skillSet.Skills()))
	}
	// Size the byte budgets from the endpoint's reported context window. The
	// window is the source of truth (Dynamo enforces it as a hard limit), so
	// these are derived, not configured. Falls back to a static model budget
	// with compaction off when the endpoint doesn't report a window (e.g.
	// Copilot).
	modelByteBudget := fallbackModelByteBudget
	contextByteBudget := 0
	if tokens, ok := aiClient.DetectContextWindowTokens(ctx); ok {
		windowBytes := tokens * avgBytesPerToken
		modelByteBudget = windowBytes * modelBudgetWindowPct / 100
		contextByteBudget = windowBytes * contextBudgetWindowPct / 100
		log.Printf("🪟 detected context window: %d tokens (~%d KB); model_byte_budget=%d KB, context_byte_budget=%d KB",
			tokens, windowBytes/1024, modelByteBudget/1024, contextByteBudget/1024)
	}
	aiBackend, err := storage.New(cfg.StorageConfig(), &http.Client{Timeout: 60 * time.Second})
	if err != nil {
		log.Printf("⚠ storage backend for AI failed (%v); AI analysis will mark failures unavailable", err)
		return
	}
	factory := artifacts.NewBackendFactory(aiBackend, cfg.Storage.Bucket)
	registry := tools.NewRegistry()
	filesystem.Register(registry)
	k8s.Register(registry)
	toolNames := eff.Tools
	if len(toolNames) == 0 {
		toolNames = []string{"filesystem", "k8s"}
	}
	enabled, err := registry.Enable(toolNames)
	if err != nil {
		// Without tools there is no analysis path; failures will be marked
		// AI-unavailable when Analyze finds no browser/registry configured.
		log.Printf("⚠ Tool registry enable failed (%v); AI analysis will mark failures unavailable", err)
	} else {
		service.EnableAgentic(ai.AgenticOptions{
			MaxIters:           eff.MaxIters,
			ModelByteBudget:    modelByteBudget,
			GCSByteBudget:      gcsByteBudget,
			Timeout:            eff.Timeout,
			ContextByteBudget:  contextByteBudget,
			MinToolCalls:       eff.MinToolCalls,
			MinGCSBytes:        eff.MinGCSBytes,
			CritiqueEnabled:    eff.Critique.Enabled,
			CritiqueMaxRetries: eff.Critique.MaxRetries,
			SingleToolCall:     eff.SingleToolCall,
			EvidenceInjection:  eff.EvidenceInjection,
		}, factory, registry, enabled)
		// Hand the loaded recipe set to the service. nil-safe; with no
		// recipes the service skips skill matching.
		service.SetSkills(skillSet)
		critiqueLog := "off"
		if eff.Critique.Enabled {
			critiqueLog = fmt.Sprintf("on/%d", eff.Critique.MaxRetries)
		}
		skillsLog := "off"
		if eff.Critique.Enabled && skillSet != nil && len(skillSet.Skills()) > 0 {
			skillsLog = fmt.Sprintf("on/%d", len(skillSet.Skills()))
		}
		log.Printf("🤖 Agentic AI enabled (%d iters, %dKB model, %dMB gcs, %s timeout, min_tools=%d, min_gcs_kb=%d, critique=%s, skills=%s, tools=%v)",
			eff.MaxIters, modelByteBudget/1024, gcsByteBudget/1024/1024, eff.Timeout, eff.MinToolCalls, eff.MinGCSBytes/1024, critiqueLog, skillsLog, enabled)
	}
	log.Printf("Using AI endpoint: %s, model: %s", aiClient.Endpoint(), aiClient.ModelName())

	var totalFailures int
	for _, d := range details {
		for _, run := range d.Runs {
			for _, tc := range run.TestCases {
				if tc.Status == "failed" {
					totalFailures++
				}
			}
		}
	}

	if totalFailures == 0 {
		log.Println("🤖 No failures to analyze")
		return
	}
	log.Printf("🤖 Analyzing %d failures...", totalFailures)

	httpClient := &http.Client{Timeout: 30 * time.Second}

	// Flatten the failed test cases into a work list so they can be analyzed
	// by a bounded worker pool. Each analysis is independent and writes only
	// its own *TestCase; the AI cache, per-build tool caches, and the
	// tools-unsupported flag are all internally synchronized, so the only
	// cross-goroutine state here is the transientSkipped counter (atomic).
	type aiWork struct {
		jobID       string
		buildPrefix string
		run         *models.BuildResult
		tc          *models.TestCase
	}
	var work []aiWork
	for i := range details {
		d := &details[i]
		jobLoc := prowbuild.JobLocation{JobType: d.JobType, Repo: d.Repo}
		for ri := range d.Runs {
			run := &d.Runs[ri]
			loc := prowbuild.BuildLocation{
				JobLocation: jobLoc,
				JobName:     d.Name,
				BuildID:     run.BuildID,
				PullNumber:  run.PullNumber,
			}
			buildPrefix := loc.BuildPath()
			for j := range run.TestCases {
				tc := &run.TestCases[j]
				if tc.Status != "failed" {
					continue
				}
				work = append(work, aiWork{jobID: d.JobID, buildPrefix: buildPrefix, run: run, tc: tc})
			}
		}
	}

	concurrency := cfg.AnalysisConcurrency()
	if concurrency > len(work) {
		concurrency = len(work)
	}
	if concurrency > 1 {
		log.Printf("🤖 analyzing with concurrency=%d", concurrency)
	}

	var transientSkipped atomic.Int64
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, w := range work {
		wg.Add(1)
		sem <- struct{}{}
		go func(w aiWork) {
			defer wg.Done()
			defer func() { <-sem }()
			before := w.tc.AISummary
			service.Analyze(ctx, httpClient, w.jobID, w.buildPrefix, w.run, w.tc)
			if before == nil && w.tc.AISummary != nil && w.tc.AISummary.IsTransient && w.tc.AIAnalysis == nil {
				transientSkipped.Add(1)
			}
		}(w)
	}
	wg.Wait()
	log.Printf("🤖 AI analysis complete (%d transient skipped)", transientSkipped.Load())

	if eff.PatternAnalysis {
		analyzePatternsAcrossBuilds(ctx, service, details)
	}
}

// patternMinFailedBuilds is the job-level "recurring" gate: a job is only
// pattern-analyzed once it has this many completed failed builds. Matches the
// consecutive-failure convention used for persistent-failure classification.
const patternMinFailedBuilds = 3

// analyzePatternsAcrossBuilds runs the opt-in second pass: for each job that
// failed in enough recent builds, it correlates one representative analyzed
// failure per failed build into a single systemic-vs-transient verdict and
// stores it on the JobDetail. Per-failure analyses are already populated.
func analyzePatternsAcrossBuilds(ctx context.Context, service *ai.Service, details []models.JobDetail) {
	for i := range details {
		d := &details[i]
		failures := gatherPatternFailures(d)
		failedBuilds := countFailedBuilds(d)
		if failedBuilds < patternMinFailedBuilds || len(failures) < 2 {
			continue
		}
		pa, err := service.AnalyzePattern(ctx, d.JobID, d.Name, failures)
		if err != nil {
			log.Printf("  ⚠ pattern analysis failed for %s: %v", d.Name, err)
			continue
		}
		if pa == nil {
			continue
		}
		d.PatternAnalyses = []models.PatternAnalysis{*pa}
		verdict := "not systemic"
		if pa.Systemic {
			verdict = fmt.Sprintf("SYSTEMIC (%s): %s", pa.Confidence, pa.SharedRootCause)
		}
		log.Printf("  🔗 pattern analysis for %s across %d builds: %s", d.Name, pa.BuildsAnalyzed, verdict)
	}
}

// countFailedBuilds counts a job's completed (non-pending) failed builds.
func countFailedBuilds(d *models.JobDetail) int {
	n := 0
	for i := range d.Runs {
		run := &d.Runs[i]
		if !run.Passed && run.Result != "PENDING" {
			n++
		}
	}
	return n
}

// gatherPatternFailures picks one representative analyzed failure per failed
// build of a job: the most severe failed test case that has an AIAnalysis. The
// transient classification is carried through (it's exactly what the pattern
// pass reconsiders across builds).
func gatherPatternFailures(d *models.JobDetail) []ai.PatternFailure {
	var out []ai.PatternFailure
	for i := range d.Runs {
		run := &d.Runs[i]
		if run.Passed || run.Result == "PENDING" {
			continue
		}
		var rep *models.TestCase
		for j := range run.TestCases {
			tc := &run.TestCases[j]
			if tc.Status != "failed" || tc.AIAnalysis == nil {
				continue
			}
			if rep == nil || severityRank(tc.AIAnalysis.Severity) > severityRank(rep.AIAnalysis.Severity) {
				rep = tc
			}
		}
		if rep == nil {
			continue
		}
		out = append(out, ai.PatternFailure{
			BuildID:        run.BuildID,
			FailingTest:    rep.Name,
			FailureMessage: rep.FailureMessage,
			RootCause:      rep.AIAnalysis.RootCause,
			IsTransient:    rep.AISummary != nil && rep.AISummary.IsTransient,
			Severity:       rep.AIAnalysis.Severity,
		})
	}
	return out
}

// severityRank orders analysis severities so the most actionable failure in a
// build is picked as its representative. Unknown/empty sorts lowest.
func severityRank(sev string) int {
	switch strings.ToLower(strings.TrimSpace(sev)) {
	case "critical":
		return 5
	case "high":
		return 4
	case "medium":
		return 3
	case "low":
		return 2
	case "transient-ignore":
		return 1
	default:
		return 0
	}
}

// aiEndpoint returns the configured AI chat-completions URL, or "" to let
// ai.NewClientWithOptions apply the Copilot default. The project.yaml
// `ai.endpoint` field wins; otherwise the AI_ENDPOINT env var is used so
// consumers can supply the value via a GitHub Actions secret rather than
// committing it to the repo.
func aiEndpoint(cfg *project.Config) string {
	if cfg.AI != nil && cfg.AI.Endpoint != "" {
		return cfg.AI.Endpoint
	}
	return os.Getenv("AI_ENDPOINT")
}

// shortHash returns a short prefix of the SkillSet hash, suitable for
// startup log lines. Empty input → empty output.
func shortHash(h string) string {
	if len(h) == 0 {
		return ""
	}
	if len(h) <= 8 {
		return h
	}
	return h[:8]
}

// aiModel returns the configured AI model identifier, or "" to let
// ai.NewClientWithOptions apply the Copilot default. The project.yaml
// `ai.model` field wins; otherwise the AI_MODEL env var is used so
// consumers can keep internal-only model labels out of the public repo.
func aiModel(cfg *project.Config) string {
	if cfg.AI != nil && cfg.AI.Model != "" {
		return cfg.AI.Model
	}
	return os.Getenv("AI_MODEL")
}

// aiHeaders returns the extra HTTP headers to attach to AI provider requests.
func aiHeaders(cfg *project.Config) map[string]string {
	if cfg.AI == nil || len(cfg.AI.Headers) == 0 {
		return nil
	}
	return cfg.AI.Headers
}
