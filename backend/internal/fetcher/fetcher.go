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
	"sort"
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
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/issues"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/junit"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/notify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/skillsuggest"
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

	// AI_TOKEN authenticates the configured chat-completions endpoint.
	aiToken := os.Getenv("AI_TOKEN")
	if opts.EnableAI && aiToken == "" {
		log.Println("Warning: -ai enabled but AI_TOKEN is not set, disabling AI analysis")
		opts.EnableAI = false
	}
	// The engine assumes no default provider: AI analysis needs an explicit
	// endpoint and model (project.yaml ai.endpoint/ai.model or AI_ENDPOINT/
	// AI_MODEL env). Fail fast on a misconfiguration rather than silently
	// publishing a dashboard with no analysis.
	if opts.EnableAI {
		if aiEndpoint(cfg) == "" || aiModel(cfg) == "" {
			return fmt.Errorf("AI is enabled but no provider is configured: set ai.endpoint and ai.model in project.yaml, or the AI_ENDPOINT and AI_MODEL env vars")
		}
	}

	// Load the prompt and skills only once AI is confirmed enabled and
	// configured, so a config error surfaces before any content errors.
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
		// Surface the systemic, job-level pattern verdicts on the home page by
		// folding them into the flakiness report (which the landing page
		// already loads). Done here, after AI attached them to details.
		flakinessReport.RecurringPatterns = collectRecurringPatterns(details)
		if n := len(flakinessReport.RecurringPatterns); n > 0 {
			log.Printf("🔗 %d systemic recurring pattern(s) surfaced on the home page", n)
		}
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

	// Step 7: auto-file GitHub issues for systemic patterns / persistent
	// failures (optional). Off unless enabled in config AND an ISSUE_TOKEN is
	// present; either missing is a no-op, never a deploy failure.
	processIssues(ctx, cfg, flakinessReport, details, opts.OutDir)

	// Step 8: draft skill-recipe PRs for systemic recurring patterns no existing
	// skill covers (optional). Off unless enabled AND AI ran AND a write-capable
	// SKILL_TOKEN is present; any missing piece is a no-op, never a failure.
	if opts.EnableAI {
		processSkillSuggestions(ctx, cfg, flakinessReport.RecurringPatterns, aiSkillSet, aiToken, opts.OutDir)
	}

	log.Println("Done!")
	return nil
}

// processIssues reconciles the project's highest-signal findings into GitHub
// issues on the configured target repo. Gated on issues.enabled + ISSUE_TOKEN.
func processIssues(ctx context.Context, cfg *project.Config, report models.FlakinessReport, details []models.JobDetail, outDir string) {
	if cfg.Issues == nil || !cfg.Issues.Enabled {
		return
	}
	token := os.Getenv("ISSUE_TOKEN")
	if token == "" {
		log.Println("Issues: enabled in config but ISSUE_TOKEN is unset; skipping")
		return
	}
	eff := cfg.EffectiveIssues()
	if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
		log.Println("Issues: no target repo resolved (set issues.repo or branding.source_repo); skipping")
		return
	}

	specs := issues.BuildSpecs(issues.BuildInput{
		Report:       report,
		JobDetails:   details,
		Triggers:     eff.Triggers,
		Labels:       eff.Labels,
		DashboardURL: cfg.Branding.SiteURL,
	})

	client := issues.NewClient(token, eff.Repo.Owner, eff.Repo.Name)
	targetRepo := eff.Repo.Owner + "/" + eff.Repo.Name
	mgr := issues.NewManager(client, filepath.Join(outDir, "issue_state.json"), targetRepo, issues.Options{
		CommentOnRecovery: eff.CommentOnRecovery == nil || *eff.CommentOnRecovery,
		CloseOnRecovery:   eff.CloseOnRecovery,
		MaxNewPerRun:      eff.MaxNewPerRun,
		RecoverPrefixes:   issues.RecoverPrefixesFor(eff.Triggers),
	})
	stats, err := mgr.Reconcile(ctx, specs)
	if err != nil {
		log.Printf("Warning: issue processing failed: %v", err)
	} else {
		log.Printf("🐙 Issues (%s/%s): %d filed, %d adopted, %d recovered",
			eff.Repo.Owner, eff.Repo.Name, stats.Created, stats.Adopted, stats.Recovered)
	}
	if err := mgr.SaveState(); err != nil {
		log.Printf("Warning: failed to save issue state: %v", err)
	}
}

// processSkillSuggestions drafts skill-recipe PRs for systemic recurring
// patterns no existing skill covers. Gated on ai.suggest_skills.enabled, a
// SKILL_TOKEN secret (contents+pull-requests write on the dashboard repo), and
// GITHUB_REPOSITORY (the dashboard repo, which GitHub Actions sets to the caller
// repo). Any missing piece is a no-op.
func processSkillSuggestions(ctx context.Context, cfg *project.Config, patterns []models.PatternAnalysis, skillSet *skills.Set, aiToken, outDir string) {
	if cfg.AI == nil || cfg.AI.SuggestSkills == nil || !cfg.AI.SuggestSkills.Enabled {
		return
	}
	if len(patterns) == 0 {
		return
	}
	ghToken := os.Getenv("SKILL_TOKEN")
	if ghToken == "" {
		log.Println("Skill suggestions: enabled but SKILL_TOKEN is unset; skipping")
		return
	}
	owner, name, ok := splitOwnerName(os.Getenv("GITHUB_REPOSITORY"))
	if !ok {
		log.Printf("Skill suggestions: GITHUB_REPOSITORY unset or invalid (%q); skipping", os.Getenv("GITHUB_REPOSITORY"))
		return
	}

	eff := cfg.EffectiveSuggestSkills()
	aiClient := ai.NewClientWithOptions(ai.Options{
		Token:        aiToken,
		Endpoint:     aiEndpoint(cfg),
		Model:        aiModel(cfg),
		ExtraHeaders: aiHeaders(cfg),
	})
	mgr := skillsuggest.NewManager(
		ghpr.NewClient(nil, ghToken), aiClient, skillSet, owner, name,
		filepath.Join(outDir, "skill_suggest_state.json"),
		skillsuggest.Options{
			MinConfidence: eff.MinConfidence,
			MaxNewPerRun:  eff.MaxNewPerRun,
			Labels:        eff.Labels,
			DashboardURL:  cfg.Branding.SiteURL,
		})
	stats, err := mgr.Reconcile(ctx, patterns)
	if err != nil {
		log.Printf("Warning: skill suggestion processing failed: %v", err)
	} else if stats.Suggested+stats.Adopted+stats.Covered > 0 {
		log.Printf("🧩 Skill suggestions (%s/%s): %d suggested, %d adopted, %d already-covered",
			owner, name, stats.Suggested, stats.Adopted, stats.Covered)
	}
	if err := mgr.SaveState(); err != nil {
		log.Printf("Warning: failed to save skill suggestion state: %v", err)
	}
}

// splitOwnerName parses an "owner/name" slug. ok is false on a malformed value.
func splitOwnerName(slug string) (owner, name string, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(slug), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
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

	// Always run the job-level pattern pass. It is self-gating (a no-op for any
	// job that didn't fail in enough builds) and cached, so it costs nothing on
	// a healthy dashboard and one cheap tool-free call per genuinely-recurring
	// job otherwise.
	analyzePatternsAcrossBuilds(ctx, service, details)
}

// patternMinFailedBuilds is the job-level "recurring" gate: a job is only
// pattern-analyzed once it has this many completed failed builds. Matches the
// consecutive-failure convention used for persistent-failure classification.
const patternMinFailedBuilds = 3

// analyzePatternsAcrossBuilds runs the cross-build pattern pass: for each job
// that failed in enough recent builds, it correlates one representative
// analyzed failure per failed build into a single systemic-vs-transient verdict
// and stores it on the JobDetail. Per-failure analyses are already populated.
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
		pa.JobID = d.JobID
		d.PatternAnalyses = []models.PatternAnalysis{*pa}
		verdict := "not systemic"
		if pa.Systemic {
			verdict = fmt.Sprintf("SYSTEMIC (%s): %s", pa.Confidence, pa.SharedRootCause)
		}
		log.Printf("  🔗 pattern analysis for %s across %d builds: %s", d.Name, pa.BuildsAnalyzed, verdict)
	}
}

// collectRecurringPatterns gathers the systemic, job-level pattern verdicts
// across all jobs into one list for the home page, ranked highest-signal first
// (by confidence, then by number of builds the verdict spans). Non-systemic
// verdicts are excluded: only confirmed recurring bugs warrant attention.
func collectRecurringPatterns(details []models.JobDetail) []models.PatternAnalysis {
	var out []models.PatternAnalysis
	for i := range details {
		for _, pa := range details[i].PatternAnalyses {
			if pa.Systemic {
				out = append(out, pa)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := confidenceRank(out[i].Confidence), confidenceRank(out[j].Confidence)
		if ri != rj {
			return ri > rj
		}
		return out[i].BuildsAnalyzed > out[j].BuildsAnalyzed
	})
	return out
}

// confidenceRank orders verdict confidences so the home-page list leads with
// the surest patterns.
func confidenceRank(c string) int {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
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

// aiEndpoint returns the configured AI chat-completions URL, or "" if neither
// is set. The project.yaml `ai.endpoint` field wins; otherwise the AI_ENDPOINT
// env var is used so consumers can supply the value via a GitHub Actions
// variable rather than committing it to the repo.
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

// aiModel returns the configured AI model identifier, or "" if neither is set.
// The project.yaml `ai.model` field wins; otherwise the AI_MODEL env var is
// used so consumers can keep internal-only model labels out of the public repo.
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
