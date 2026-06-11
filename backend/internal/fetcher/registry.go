package fetcher

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/collectors"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// CollectorFactory constructs a collectors.Collector from project config and
// the shared deps every collector needs (GCS bucket, HTTP client). Factories
// are registered with a *CollectorRegistry at startup; cmd/fetcher wires the
// built-in factories explicitly so tests can compose their own registries.
type CollectorFactory func(cfg *project.Config, bucket *gcs.Bucket, client *http.Client) (collectors.Collector, error)

// CollectorRegistry maps a collector name (project.yaml artifacts.collector)
// to its factory. The zero value is not usable; use NewCollectorRegistry.
type CollectorRegistry struct {
	factories map[string]CollectorFactory
}

// NewCollectorRegistry returns an empty registry.
func NewCollectorRegistry() *CollectorRegistry {
	return &CollectorRegistry{factories: map[string]CollectorFactory{}}
}

// Register adds a factory under name. Panics on duplicate registration since
// that is always a programming error.
func (r *CollectorRegistry) Register(name string, f CollectorFactory) {
	if _, exists := r.factories[name]; exists {
		panic(fmt.Sprintf("collector %q already registered", name))
	}
	r.factories[name] = f
}

// Has reports whether a factory is registered under name.
func (r *CollectorRegistry) Has(name string) bool {
	_, ok := r.factories[name]
	return ok
}

// Names returns the sorted list of registered collector names.
func (r *CollectorRegistry) Names() []string {
	names := make([]string, 0, len(r.factories))
	for n := range r.factories {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Build picks the factory named by cfg.CollectorName() and invokes it. The
// error message lists registered alternatives so misconfigurations point
// users at the fix.
func (r *CollectorRegistry) Build(cfg *project.Config, bucket *gcs.Bucket, client *http.Client) (collectors.Collector, error) {
	name := cfg.CollectorName()
	f, ok := r.factories[name]
	if !ok {
		return nil, fmt.Errorf("unknown artifacts.collector %q (registered: %s)", name, strings.Join(r.Names(), ", "))
	}
	return f(cfg, bucket, client)
}
