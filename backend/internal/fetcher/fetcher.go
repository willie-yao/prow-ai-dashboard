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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aggregator"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/collectors"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcsweb"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/junit"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/notify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
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
	PeriodicOnly bool
	EnableAI     bool
	Collectors   *CollectorRegistry
	AIModules    *AIModuleRegistry
}

// Run executes the full pipeline: load → discover → fetch → aggregate →
// optionally analyze with AI → write output → optionally notify. Per-job
// fetch errors are logged but do not abort the run; only startup failures
// (config load, collector wiring) return an error.
func Run(ctx context.Context, opts Options) error {
	if opts.Collectors == nil {
		return fmt.Errorf("fetcher.Options.Collectors registry is required")
	}
	if opts.AIModules == nil {
		return fmt.Errorf("fetcher.Options.AIModules registry is required")
	}

	cfg, err := project.Load(filepath.Join(opts.ProjectDir, "project.yaml"))
	if err != nil {
		return fmt.Errorf("loading project config: %w", err)
	}
	log.Printf("Project: %s (%s) dashboard=%s bucket=%s",
		cfg.Name, cfg.DisplayShortName(), cfg.TestGrid.Dashboard, cfg.GCS.Bucket)

	// Validate plugin references against the registries up front so a typo'd
	// artifacts.collector or ai.module fails before any expensive work.
	// ai.module is checked regardless of -ai so misconfigurations surface in
	// every run, matching the old project.Validate behavior.
	if !opts.Collectors.Has(cfg.CollectorName()) {
		return fmt.Errorf("unknown artifacts.collector %q (registered: %s)",
			cfg.CollectorName(), strings.Join(opts.Collectors.Names(), ", "))
	}
	if cfg.AI != nil && strings.TrimSpace(cfg.AI.Module) != "" && !opts.AIModules.Has(cfg.AI.Module) {
		return fmt.Errorf("unknown ai.module %q (registered: %s)",
			cfg.AI.Module, strings.Join(opts.AIModules.Names(), ", "))
	}

	var aiSystemPrompt string
	if opts.EnableAI {
		_, prompt, err := project.LoadDir(opts.ProjectDir)
		if err != nil {
			return fmt.Errorf("loading AI prompt: %w", err)
		}
		aiSystemPrompt = ai.ComposeSystemPrompt(prompt)
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
	bucket := gcs.NewBucket(cfg.GCS.Bucket)

	collector, err := opts.Collectors.Build(cfg, bucket, client)
	if err != nil {
		return fmt.Errorf("building collector: %w", err)
	}
	log.Printf("Using artifact collector: %s", collector.Name())

	// Step 1: Discover jobs from test-infra config YAMLs.
	log.Println("Fetching job configs from test-infra...")
	jobs, err := jobconfig.FetchJobConfigs(ctx, client, cfg)
	if err != nil {
		return fmt.Errorf("fetching job configs: %w", err)
	}

	if opts.PeriodicOnly {
		var periodic []models.ProwJob
		for _, j := range jobs {
			if j.MinimumInterval != "" {
				periodic = append(periodic, j)
			}
		}
		jobs = periodic
	}
	log.Printf("Discovered %d jobs", len(jobs))

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

			runs, err := fetchJobRunsCached(ctx, client, bucket, cfg, collector, j.Name, opts.BuildsPerJob, cachedJobs[j.Name])
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
		dashboard.Jobs = append(dashboard.Jobs, aggregator.ComputeJobSummary(r.job, r.runs, now))
		details = append(details, models.JobDetail{Name: r.job.Name, Runs: r.runs})
	}

	// Step 4: Flakiness report + search index.
	jobResultMap := make(map[string][]models.BuildResult, len(results))
	for _, r := range results {
		if r.job.Name == "" {
			continue
		}
		jobResultMap[r.job.Name] = r.runs
	}
	flakinessReport := aggregator.ComputeFlakinessReport(jobResultMap, now)
	log.Printf("Flakiness report: %d most flaky, %d persistent, %d recently broken",
		len(flakinessReport.MostFlaky), len(flakinessReport.PersistentFailures), len(flakinessReport.RecentlyBroken))

	searchIndex := aggregator.BuildSearchIndex(jobResultMap, jobs, now)
	log.Printf("Search index: %d entries", len(searchIndex.Entries))

	// Step 5: AI failure analysis (optional).
	if opts.EnableAI {
		analyzeFailuresWithAI(ctx, cfg, opts.AIModules, details, flakinessReport, aiToken, opts.OutDir, aiSystemPrompt)
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
			bucket.ProwURL(""),
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

// loadCachedJobDetails loads existing per-job JSON files from the output dir
// and returns a map of job name → cached BuildResults (keyed by build ID).
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
		if json.Unmarshal(data, &detail) != nil || detail.Name == "" {
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
			cached[detail.Name] = builds
		}
	}
	return cached
}

// fetchJobRunsCached discovers recent builds and reuses cached data for
// completed builds. Per-build fetch errors are logged but do not abort.
func fetchJobRunsCached(ctx context.Context, client *http.Client, bucket *gcs.Bucket, cfg *project.Config, collector collectors.Collector, jobName string, count int, cachedBuilds map[string]models.BuildResult) ([]models.BuildResult, error) {
	buildIDs, err := gcsweb.ListRecentBuildIDs(ctx, client, bucket, jobName, count)
	if err != nil {
		return nil, fmt.Errorf("listing builds: %w", err)
	}

	var runs []models.BuildResult
	fetched, reused := 0, 0
	for _, bid := range buildIDs {
		if cached, ok := cachedBuilds[bid]; ok {
			runs = append(runs, cached)
			reused++
			continue
		}
		result, err := fetchBuildResult(ctx, client, bucket, cfg, collector, jobName, bid)
		if err != nil {
			log.Printf("    ⚠ %s/%s: %v", jobName, bid, err)
			continue
		}
		runs = append(runs, *result)
		fetched++
	}

	if reused > 0 {
		log.Printf("    💾 %s: %d cached, %d fetched", jobName, reused, fetched)
	}

	return runs, nil
}

// fetchBuildResult fetches metadata and JUnit XML for a single build, then
// delegates per-test artifact discovery to the configured collector.
func fetchBuildResult(ctx context.Context, client *http.Client, bucket *gcs.Bucket, _ *project.Config, collector collectors.Collector, jobName, buildID string) (*models.BuildResult, error) {
	info, err := gcs.FetchBuildInfo(ctx, client, bucket, jobName, buildID)
	if err != nil {
		return nil, fmt.Errorf("fetching build info: %w", err)
	}

	result := &models.BuildResult{BuildInfo: *info, TestCases: []models.TestCase{}}

	// JUnit XML is best-effort — some builds may not have it.
	junitData, err := gcs.FetchRaw(ctx, client, info.JUnitURL)
	if err != nil {
		return result, nil
	}

	testCases, err := junit.Parse(junitData)
	if err != nil {
		log.Printf("    ⚠ %s/%s: failed to parse JUnit: %v", jobName, buildID, err)
		return result, nil
	}

	result.TestCases = testCases
	for _, tc := range testCases {
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

	if err := collector.CollectArtifacts(ctx, jobName, buildID, result); err != nil {
		log.Printf("    ⚠ %s/%s: collector %s: %v", jobName, buildID, collector.Name(), err)
	}

	return result, nil
}

// analyzeFailuresWithAI runs AI analysis on every failed test case.
func analyzeFailuresWithAI(ctx context.Context, cfg *project.Config, modules *AIModuleRegistry, details []models.JobDetail, flakinessReport models.FlakinessReport, token, outDir, systemPrompt string) {
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
		consecutiveMap[tf.TestName] = tf.ConsecutiveFailures
	}

	module, err := modules.Build(cfg)
	if err != nil {
		// AI module registry was already validated in Run; reaching here
		// means a programming error in fallback wiring rather than a
		// recoverable config issue.
		log.Printf("AI module build failed: %v; skipping AI analysis", err)
		return
	}
	service := ai.NewService(aiClient, module, systemPrompt, consecutiveMap)
	log.Printf("Using AI module: %s, endpoint: %s, model: %s", module.Name(), aiClient.Endpoint(), aiClient.ModelName())

	var totalFailures, transientSkipped int
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

	for i := range details {
		for ri := range details[i].Runs {
			run := &details[i].Runs[ri]
			for j := range run.TestCases {
				tc := &run.TestCases[j]
				if tc.Status != "failed" {
					continue
				}
				before := tc.AISummary
				service.Analyze(ctx, httpClient, run, tc)
				if before == nil && tc.AISummary != nil && tc.AISummary.IsTransient && tc.AIAnalysis == nil {
					transientSkipped++
				}
			}
		}
	}
	log.Printf("🤖 AI analysis complete (%d transient skipped)", transientSkipped)
}

// aiEndpoint returns the configured AI chat-completions URL, or "" to let
// ai.NewClientWithOptions apply the Copilot default.
func aiEndpoint(cfg *project.Config) string {
	if cfg.AI == nil {
		return ""
	}
	return cfg.AI.Endpoint
}

// aiModel returns the configured AI model identifier, or "" to let
// ai.NewClientWithOptions apply the Copilot default.
func aiModel(cfg *project.Config) string {
	if cfg.AI == nil {
		return ""
	}
	return cfg.AI.Model
}

// aiHeaders returns the extra HTTP headers to attach to AI provider requests.
func aiHeaders(cfg *project.Config) map[string]string {
	if cfg.AI == nil || len(cfg.AI.Headers) == 0 {
		return nil
	}
	return cfg.AI.Headers
}
