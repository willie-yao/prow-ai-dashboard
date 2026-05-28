package capi

import (
	"net/http"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/collectors"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// Factory wires this collector for fetcher.CollectorRegistry. cmd/fetcher
// registers it explicitly so tests can compose their own registries. The
// capi section in project.yaml is optional; without it the collector still
// works but skips per-test namespace mapping (CAPI core convention).
func Factory(cfg *project.Config, bucket *gcs.Bucket, client *http.Client) (collectors.Collector, error) {
	prefix := ""
	if cfg.CAPI != nil {
		prefix = cfg.CAPI.ClusterNamePrefix
	}
	return New(bucket, client, prefix)
}
