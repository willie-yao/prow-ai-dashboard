// Command fetcher is the dashboard data pipeline. It loads a project
// configuration, discovers Prow jobs, fetches build results from GCS, runs
// optional AI failure analysis, and writes JSON for the frontend to render.
// All orchestration lives in internal/fetcher; this file is just flag
// parsing and the explicit wiring of the built-in collector factory into the
// fetcher's collector registry.
//
// The `onboard` subcommand scaffolds a new dashboard config from a testgrid
// dashboard name or a storage bucket (see internal/onboard).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	collectorgeneric "github.com/willie-yao/prow-ai-dashboard/backend/internal/collectors/generic"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetcher"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/onboard"
)

// version is the engine version, overridden at build time via
// -ldflags "-X main.version=<tag>". Defaults to "dev" for local builds.
var version = "dev"

func main() {
	// Subcommand dispatch: `fetcher onboard ...` scaffolds a new project;
	// everything else is the default data-pipeline run.
	if len(os.Args) > 1 && os.Args[1] == "onboard" {
		runOnboard(os.Args[2:])
		return
	}

	var opts fetcher.Options
	flag.StringVar(&opts.ProjectDir, "project-dir", ".", "directory containing project.yaml and prompts/system.md")
	flag.StringVar(&opts.OutDir, "out", "data", "output directory for JSON files")
	flag.IntVar(&opts.BuildsPerJob, "builds", 10, "number of recent builds to fetch per job")
	flag.IntVar(&opts.Workers, "workers", 5, "number of concurrent job fetchers")
	flag.DurationVar(&opts.Timeout, "timeout", 10*time.Minute, "overall fetch timeout")
	flag.BoolVar(&opts.IncludePresubmits, "include-presubmits", false, "include presubmit jobs in addition to periodics (ORed with project.yaml source.include_presubmits)")
	flag.BoolVar(&opts.EnableAI, "ai", false, "enable AI-powered failure analysis")
	flag.Parse()

	opts.Version = version
	opts.Collectors = fetcher.NewCollectorRegistry()
	opts.Collectors.Register("generic", collectorgeneric.Factory)

	if err := fetcher.Run(context.Background(), opts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// runOnboard parses the onboard subcommand flags and scaffolds a new dashboard.
func runOnboard(args []string) {
	fs := flag.NewFlagSet("onboard", flag.ExitOnError)
	var opts onboard.Options
	fs.StringVar(&opts.TestGrid, "testgrid", "", "testgrid dashboard name to discover jobs from (kubernetes-ecosystem Prow)")
	fs.StringVar(&opts.Bucket, "bucket", "", "artifact bucket name for bucket-based discovery (any Prow); alternative to -testgrid")
	fs.StringVar(&opts.GCSWebBase, "gcsweb-base", "", "gcsweb gateway root for the bucket (e.g. https://gcsweb.istio.io/s3); selects the gcsweb provider")
	fs.StringVar(&opts.DashboardRepo, "dashboard-repo", "", "owner/name of the repo that will publish the dashboard (required)")
	fs.StringVar(&opts.SourceRepo, "source-repo", "", "owner/name of the code repo under test (required)")
	fs.StringVar(&opts.ID, "id", "", "project id (default: derived from the dashboard repo name)")
	fs.StringVar(&opts.Name, "name", "", "project display name (default: derived from the id)")
	fs.BoolVar(&opts.IncludePresubmits, "include-presubmits", false, "include presubmit jobs in the sweep")
	fs.StringVar(&opts.EngineRef, "engine-ref", "main", "prow-ai-dashboard ref the generated workflows pin")
	fs.StringVar(&opts.OutDir, "out", "", "output directory for the scaffold (default: the dashboard repo name)")
	fs.BoolVar(&opts.NoPrompt, "no-prompt", false, "skip AI prompt drafting and always write the prompts/system.md stub")
	fs.BoolVar(&opts.OpenPR, "open-pr", false, "open a PR against the dashboard repo with the scaffold instead of writing a local directory (needs GITHUB_TOKEN write access)")
	_ = fs.Parse(args)

	// AI_TOKEN authenticates the chat-completions endpoint (prompt drafting);
	// GITHUB_TOKEN reads the source repo's docs.
	opts.AIToken = os.Getenv("AI_TOKEN")
	opts.AIEndpoint = os.Getenv("AI_ENDPOINT")
	opts.AIModel = os.Getenv("AI_MODEL")
	opts.GitHubToken = os.Getenv("GITHUB_TOKEN")

	if err := onboard.Run(context.Background(), opts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
