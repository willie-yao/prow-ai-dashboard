package generic

import (
	"net/http"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/collectors"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

// Factory wires this collector for fetcher.CollectorRegistry.
func Factory(_ *project.Config, _ storage.Backend, _ *http.Client) (collectors.Collector, error) {
	return New(), nil
}
