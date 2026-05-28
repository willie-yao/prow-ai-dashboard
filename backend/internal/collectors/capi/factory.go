package capi

import (
	"log"
	"net/http"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/collectors"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// Factory wires this collector for fetcher.CollectorRegistry. cmd/fetcher
// registers it explicitly so tests can compose their own registries. The
// capi section in project.yaml is optional; without it the collector still
// works but skips per-test namespace mapping (CAPI core convention).
// Controller log selectors come from cfg.AI.Evidence with engine defaults
// applied via project.Config.EffectiveEvidence.
func Factory(cfg *project.Config, bucket *gcs.Bucket, client *http.Client) (collectors.Collector, error) {
	prefix := ""
	if cfg.CAPI != nil {
		prefix = cfg.CAPI.ClusterNamePrefix
	}
	ev, err := cfg.EffectiveEvidence()
	if err != nil {
		// project.Config.Validate already surfaces evidence regex errors at
		// load time. Reaching this branch implies a programmer bug; log and
		// fall back to engine defaults so the dashboard keeps deploying.
		log.Printf("⚠ collectors.capi: unexpected evidence config error: %v; using defaults", err)
		fallback := &project.Config{}
		ev, _ = fallback.EffectiveEvidence()
	}
	return New(bucket, client, prefix, ev.ControllerLogs, ev.PodNameRegexes)
}
