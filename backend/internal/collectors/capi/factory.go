package capi

import (
	"fmt"
	"net/http"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/collectors"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// Factory wires this collector for fetcher.CollectorRegistry. cmd/fetcher
// registers it explicitly so tests can compose their own registries.
func Factory(cfg *project.Config, bucket *gcs.Bucket, client *http.Client) (collectors.Collector, error) {
	if cfg.CAPI == nil {
		return nil, fmt.Errorf("artifacts.collector=capi requires a capi section in project.yaml")
	}
	return New(bucket, client, cfg.CAPI.ClusterNamePrefix)
}
