package generic

import (
	"net/http"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/collectors"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// Factory wires this collector for fetcher.CollectorRegistry.
func Factory(_ *project.Config, _ *gcs.Bucket, _ *http.Client) (collectors.Collector, error) {
	return New(), nil
}
