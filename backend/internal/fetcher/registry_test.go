package fetcher

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/collectors"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

type stubCollector struct{ name string }

func (s *stubCollector) Name() string { return s.name }
func (s *stubCollector) CollectArtifacts(_ context.Context, _ gcs.BuildLocation, _ *models.BuildResult) error {
	return nil
}

type stubModule struct{ name string }

func (s *stubModule) Name() string                     { return s.name }
func (s *stubModule) IsKnownTransient(_ string) string { return "" }
func (s *stubModule) AnalysisPrompt(_ context.Context, _ *http.Client, _ *models.BuildResult, _ *models.TestCase, _ int) string {
	return ""
}

func TestCollectorRegistry_BuildAndNames(t *testing.T) {
	r := NewCollectorRegistry()
	r.Register("kubernetes", func(_ *project.Config, _ *gcs.Bucket, _ *http.Client) (collectors.Collector, error) {
		return &stubCollector{name: "kubernetes"}, nil
	})
	r.Register("generic", func(_ *project.Config, _ *gcs.Bucket, _ *http.Client) (collectors.Collector, error) {
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
	r.Register("generic", func(_ *project.Config, _ *gcs.Bucket, _ *http.Client) (collectors.Collector, error) {
		return &stubCollector{name: "generic"}, nil
	})

	// No artifacts section → CollectorName() returns "generic".
	cfg := &project.Config{}
	got, err := r.Build(cfg, nil, nil)
	if err != nil || got.Name() != "generic" {
		t.Fatalf("Build default: got=%v err=%v", got, err)
	}
}

func TestCollectorRegistry_DuplicatePanics(t *testing.T) {
	r := NewCollectorRegistry()
	f := func(_ *project.Config, _ *gcs.Bucket, _ *http.Client) (collectors.Collector, error) {
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

func TestAIModuleRegistry_ExplicitChoice(t *testing.T) {
	r := NewAIModuleRegistry()
	r.Register("generic", func(_ *project.Config) ai.Module { return &stubModule{name: "generic"} })
	r.Register("kubernetes", func(_ *project.Config) ai.Module { return &stubModule{name: "kubernetes"} })

	cfg := &project.Config{AI: &project.AI{Module: "kubernetes"}}
	got, err := r.Build(cfg)
	if err != nil || got.Name() != "kubernetes" {
		t.Errorf("explicit kubernetes: got=%v err=%v", got, err)
	}

	cfg.AI.Module = "missing"
	_, err = r.Build(cfg)
	if err == nil || !strings.Contains(err.Error(), "registered: generic, kubernetes") {
		t.Errorf("explicit unknown should error with registered list: %v", err)
	}
}

func TestAIModuleRegistry_FallbackToGeneric(t *testing.T) {
	r := NewAIModuleRegistry()
	r.Register("generic", func(_ *project.Config) ai.Module { return &stubModule{name: "generic"} })

	cfg := &project.Config{}
	got, err := r.Build(cfg)
	if err != nil || got.Name() != "generic" {
		t.Errorf("unset ai.module falls back to generic: got=%v err=%v", got, err)
	}
}

func TestAIModuleRegistry_NoGenericIsError(t *testing.T) {
	r := NewAIModuleRegistry()
	// Don't register "generic". Any implicit lookup should error.
	cfg := &project.Config{}
	_, err := r.Build(cfg)
	if err == nil || !strings.Contains(err.Error(), `"generic"`) {
		t.Errorf("missing generic should error: %v", err)
	}
}
