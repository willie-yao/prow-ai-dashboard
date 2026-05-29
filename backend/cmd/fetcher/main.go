// Command fetcher is the dashboard data pipeline. It loads a project
// configuration, discovers Prow jobs, fetches build results from GCS, runs
// optional AI failure analysis, and writes JSON for the frontend to render.
// All orchestration lives in internal/fetcher; this file is just flag
// parsing and the explicit wiring of the built-in collector and AI module
// factories into the fetcher registries.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	aicapi "github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/modules/capi"
	aigeneric "github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/modules/generic"
	collectorcapi "github.com/willie-yao/prow-ai-dashboard/backend/internal/collectors/capi"
	collectorgeneric "github.com/willie-yao/prow-ai-dashboard/backend/internal/collectors/generic"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetcher"
)

func main() {
	var opts fetcher.Options
	flag.StringVar(&opts.ProjectDir, "project-dir", ".", "directory containing project.yaml and prompts/system.md")
	flag.StringVar(&opts.OutDir, "out", "data", "output directory for JSON files")
	flag.IntVar(&opts.BuildsPerJob, "builds", 10, "number of recent builds to fetch per job")
	flag.IntVar(&opts.Workers, "workers", 5, "number of concurrent job fetchers")
	flag.DurationVar(&opts.Timeout, "timeout", 10*time.Minute, "overall fetch timeout")
	flag.BoolVar(&opts.IncludePresubmits, "include-presubmits", false, "include presubmit jobs in addition to periodics (ORed with project.yaml source.include_presubmits)")
	flag.BoolVar(&opts.EnableAI, "ai", false, "enable AI-powered failure analysis")
	flag.Parse()

	opts.Collectors = fetcher.NewCollectorRegistry()
	opts.Collectors.Register("capi", collectorcapi.Factory)
	opts.Collectors.Register("generic", collectorgeneric.Factory)

	opts.AIModules = fetcher.NewAIModuleRegistry()
	opts.AIModules.Register("capi", aicapi.Factory)
	opts.AIModules.Register("generic", aigeneric.Factory)

	if err := fetcher.Run(context.Background(), opts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
