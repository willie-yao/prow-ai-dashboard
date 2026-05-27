package main

import (
	"context"
	"encoding/json"
	"flag"
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
	aicapi "github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/modules/capi"
	aigeneric "github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/modules/generic"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/collectors"
	collectorcapi "github.com/willie-yao/prow-ai-dashboard/backend/internal/collectors/capi"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/collectors/generic"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcsweb"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/junit"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/notify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	projectDir := flag.String("project-dir", ".", "directory containing project.yaml and prompts/system.md")
	outDir := flag.String("out", "data", "output directory for JSON files")
	buildsPerJob := flag.Int("builds", 10, "number of recent builds to fetch per job")
	workers := flag.Int("workers", 5, "number of concurrent job fetchers")
	timeout := flag.Duration("timeout", 10*time.Minute, "overall fetch timeout")
	periodicOnly := flag.Bool("periodic-only", true, "only fetch periodic jobs (skip presubmits)")
	enableAI := flag.Bool("ai", false, "enable AI-powered failure analysis")
	flag.Parse()

	// project.yaml is always loaded. prompts/system.md is only required when
	// -ai is set; without AI the dashboard still produces a useful Prow view.
	cfg, err := project.Load(filepath.Join(*projectDir, "project.yaml"))
	if err != nil {
		return fmt.Errorf("loading project config: %w", err)
	}
	log.Printf("Project: %s (%s) dashboard=%s bucket=%s",
		cfg.Name, cfg.DisplayShortName(), cfg.TestGrid.Dashboard, cfg.GCS.Bucket)

	var aiSystemPrompt string
	if *enableAI {
		_, prompt, err := project.LoadDir(*projectDir)
		if err != nil {
			return fmt.Errorf("loading AI prompt: %w", err)
		}
		aiSystemPrompt = ai.ComposeSystemPrompt(prompt)
	}

	aiToken := os.Getenv("AI_TOKEN")
	if *enableAI && aiToken == "" {
		aiToken = os.Getenv("GITHUB_TOKEN")
	}
	if *enableAI && aiToken == "" {
		log.Println("Warning: -ai enabled but no AI_TOKEN or GITHUB_TOKEN set, disabling AI analysis")
		*enableAI = false
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	client := &http.Client{Timeout: 30 * time.Second}
	bucket := gcs.NewBucket(cfg.GCS.Bucket)

	collector, err := buildCollector(cfg, bucket, client)
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

	if *periodicOnly {
		var periodic []models.ProwJob
		for _, j := range jobs {
			if j.MinimumInterval != "" {
				periodic = append(periodic, j)
			}
		}
		jobs = periodic
	}
	log.Printf("Discovered %d jobs", len(jobs))

	// Step 2: For each job, discover builds and fetch results.
	// Load existing data to skip re-fetching completed builds.
	cachedJobs := loadCachedJobDetails(*outDir)

	type jobResult struct {
		job  models.ProwJob
		runs []models.BuildResult
	}

	results := make([]jobResult, len(jobs))
	sem := make(chan struct{}, *workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var fetchErrors []error

	for i, job := range jobs {
		wg.Add(1)
		go func(idx int, j models.ProwJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			runs, err := fetchJobRunsCached(ctx, client, bucket, cfg, collector, j.Name, *buildsPerJob, cachedJobs[j.Name])
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

	// Step 3: Aggregate and write output.
	now := time.Now().UTC()
	dashboard := models.Dashboard{
		GeneratedAt: now,
	}
	var details []models.JobDetail

	for _, r := range results {
		if r.job.Name == "" {
			continue // skipped due to fetch error
		}

		summary := aggregator.ComputeJobSummary(r.job, r.runs, now)
		dashboard.Jobs = append(dashboard.Jobs, summary)

		detail := models.JobDetail{
			Name: r.job.Name,
			Runs: r.runs,
		}
		details = append(details, detail)
	}

	// Step 4: Compute flakiness report.
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

	// Step 4b: Build search index.
	searchIndex := aggregator.BuildSearchIndex(jobResultMap, jobs, now)
	log.Printf("Search index: %d entries", len(searchIndex.Entries))

	// Step 5: AI failure analysis (optional).
	if *enableAI {
		analyzeFailuresWithAI(ctx, cfg, details, flakinessReport, aiToken, *outDir, aiSystemPrompt)
	}

	log.Printf("Writing output to %s/ (%d jobs)", *outDir, len(dashboard.Jobs))
	if err := output.WriteAll(*outDir, cfg, dashboard, details, flakinessReport, searchIndex); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}

	// Step 6: Slack/Teams notifications for persistent failures (optional).
	slackWebhookURL := os.Getenv("SLACK_WEBHOOK_URL")
	if slackWebhookURL != "" {
		notifier := notify.NewNotifier(
			slackWebhookURL,
			filepath.Join(*outDir, "notification_state.json"),
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

// buildCollector chooses an artifact collector based on cfg.Artifacts.Collector.
// New collector implementations are wired in here.
func buildCollector(cfg *project.Config, bucket *gcs.Bucket, client *http.Client) (collectors.Collector, error) {
	name := cfg.CollectorName()
	switch name {
	case "generic":
		return generic.New(), nil
	case "capi":
		if cfg.CAPI == nil {
			return nil, fmt.Errorf("artifacts.collector=capi requires a capi section in project.yaml")
		}
		return collectorcapi.New(bucket, client, cfg.CAPI.ClusterNamePrefix)
	default:
		return nil, fmt.Errorf("unknown artifacts.collector %q", name)
	}
}

// fetchJobRunsCached discovers recent builds and uses cached data for completed builds.
func fetchJobRunsCached(ctx context.Context, client *http.Client, bucket *gcs.Bucket, cfg *project.Config, collector collectors.Collector, jobName string, count int, cachedBuilds map[string]models.BuildResult) ([]models.BuildResult, error) {
	buildIDs, err := gcsweb.ListRecentBuildIDs(ctx, client, bucket, jobName, count)
	if err != nil {
		return nil, fmt.Errorf("listing builds: %w", err)
	}

	var runs []models.BuildResult
	fetched, reused := 0, 0
	for _, bid := range buildIDs {
		// Use cached data for completed builds.
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

// fetchJobRuns discovers recent builds for a job and fetches their results.
func fetchJobRuns(ctx context.Context, client *http.Client, bucket *gcs.Bucket, cfg *project.Config, collector collectors.Collector, jobName string, count int) ([]models.BuildResult, error) {
	buildIDs, err := gcsweb.ListRecentBuildIDs(ctx, client, bucket, jobName, count)
	if err != nil {
		return nil, fmt.Errorf("listing builds: %w", err)
	}

	var runs []models.BuildResult
	for _, bid := range buildIDs {
		result, err := fetchBuildResult(ctx, client, bucket, cfg, collector, jobName, bid)
		if err != nil {
			log.Printf("    ⚠ %s/%s: %v", jobName, bid, err)
			continue
		}
		runs = append(runs, *result)
	}

	return runs, nil
}

// fetchBuildResult fetches metadata and JUnit XML for a single build,
// then delegates per-test artifact discovery to the configured collector.
func fetchBuildResult(ctx context.Context, client *http.Client, bucket *gcs.Bucket, cfg *project.Config, collector collectors.Collector, jobName, buildID string) (*models.BuildResult, error) {
	info, err := gcs.FetchBuildInfo(ctx, client, bucket, jobName, buildID)
	if err != nil {
		return nil, fmt.Errorf("fetching build info: %w", err)
	}

	result := &models.BuildResult{
		BuildInfo: *info,
	}

	// Fetch JUnit XML (best-effort — some builds may not have it).
	junitData, err := gcs.FetchRaw(ctx, client, info.JUnitURL)
	if err != nil {
		// JUnit not available — return result with metadata only.
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

// analyzeFailuresWithAI runs AI analysis on failed test cases.
func analyzeFailuresWithAI(ctx context.Context, cfg *project.Config, details []models.JobDetail, flakinessReport models.FlakinessReport, token, outDir, systemPrompt string) {
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

	// Build a lookup of consecutive failure counts from the flakiness report.
	consecutiveMap := make(map[string]int)
	for _, tf := range flakinessReport.PersistentFailures {
		consecutiveMap[tf.TestName] = tf.ConsecutiveFailures
	}

	module := buildAIModule(cfg)
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
				// Count transient short-circuits for the summary log.
				if before == nil && tc.AISummary != nil && tc.AISummary.IsTransient && tc.AIAnalysis == nil {
					transientSkipped++
				}
			}
		}
	}
	log.Printf("🤖 AI analysis complete (%d transient skipped)", transientSkipped)
}

// buildAIModule selects the AI module implementation based on project config.
func buildAIModule(cfg *project.Config) ai.Module {
	switch cfg.AIModuleName() {
	case "capi":
		prefix := ""
		if cfg.CAPI != nil {
			prefix = cfg.CAPI.ClusterNamePrefix
		}
		return aicapi.New(prefix)
	default:
		return aigeneric.New()
	}
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
