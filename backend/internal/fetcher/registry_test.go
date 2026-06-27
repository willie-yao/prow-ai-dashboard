package fetcher

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/collectors"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

type stubCollector struct{ name string }

func (s *stubCollector) Name() string { return s.name }
func (s *stubCollector) CollectArtifacts(_ context.Context, _ prowbuild.BuildLocation, _ *models.BuildResult) error {
	return nil
}

func TestCollectorRegistry_BuildAndNames(t *testing.T) {
	r := NewCollectorRegistry()
	r.Register("kubernetes", func(_ *project.Config, _ storage.Backend, _ *http.Client) (collectors.Collector, error) {
		return &stubCollector{name: "kubernetes"}, nil
	})
	r.Register("generic", func(_ *project.Config, _ storage.Backend, _ *http.Client) (collectors.Collector, error) {
		return &stubCollector{name: "generic"}, nil
	})

	if names := r.Names(); strings.Join(names, ",") != "generic,kubernetes" {
		t.Errorf("Names = %v, want sorted [generic kubernetes]", names)
	}
	if !r.Has("kubernetes") || r.Has("missing") {
		t.Errorf("Has wrong: kubernetes=%v missing=%v", r.Has("kubernetes"), r.Has("missing"))
	}

	cfg := &project.Config{Artifacts: &project.Artifacts{Collector: "kubernetes"}}
	got, err := r.Build(cfg, nil, nil)
	if err != nil || got.Name() != "kubernetes" {
		t.Fatalf("Build(kubernetes): got=%v err=%v", got, err)
	}

	cfg.Artifacts.Collector = "ghost"
	_, err = r.Build(cfg, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "registered: generic, kubernetes") {
		t.Errorf("unknown collector error should list registered: %v", err)
	}
}

func TestCollectorRegistry_DefaultsToGeneric(t *testing.T) {
	r := NewCollectorRegistry()
	r.Register("generic", func(_ *project.Config, _ storage.Backend, _ *http.Client) (collectors.Collector, error) {
		return &stubCollector{name: "generic"}, nil
	})

	// Missing artifacts section defaults CollectorName to "generic".
	cfg := &project.Config{}
	got, err := r.Build(cfg, nil, nil)
	if err != nil || got.Name() != "generic" {
		t.Fatalf("Build default: got=%v err=%v", got, err)
	}
}

func TestCollectorRegistry_DuplicatePanics(t *testing.T) {
	r := NewCollectorRegistry()
	f := func(_ *project.Config, _ storage.Backend, _ *http.Client) (collectors.Collector, error) {
		return &stubCollector{name: "x"}, nil
	}
	r.Register("x", f)

	defer func() {
		if recover() == nil {
			t.Errorf("expected panic on duplicate registration")
		}
	}()
	r.Register("x", f)
}
