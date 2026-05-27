package project

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestLoadCAPZGolden verifies that the canonical configs/capz/project.yaml
// in this repo parses and validates. This is the golden test that pins
// CAPZ's config values during Phase A while behavior must stay identical.
func TestLoadCAPZGolden(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// backend/internal/project/golden_test.go -> ../../../configs/capz/project.yaml
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "configs", "capz", "project.yaml")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%s): %v", path, err)
	}

	checks := []struct {
		name string
		got  string
		want string
	}{
		{"id", cfg.ID, "capz"},
		{"short_name", cfg.ShortName, "CAPZ"},
		{"source.test_infra_path", cfg.Source.TestInfraPath, "config/jobs/kubernetes-sigs/cluster-api-provider-azure"},
		{"source.file_prefix", cfg.Source.FilePrefix, "cluster-api-provider-azure-"},
		{"testgrid.dashboard", cfg.TestGrid.Dashboard, "sig-cluster-lifecycle-cluster-api-provider-azure"},
		{"gcs.bucket", cfg.GCS.Bucket, "kubernetes-ci-logs"},
		{"branding.title", cfg.Branding.Title, "CAPZ Prow Dashboard"},
		{"branding.base_path", cfg.Branding.BasePath, "/capz-prow-ai-dashboard"},
		{"branding.site_url", cfg.Branding.SiteURL, "https://willie-yao.github.io/capz-prow-ai-dashboard"},
		{"branding.source_repo.owner", cfg.Branding.SourceRepo.Owner, "kubernetes-sigs"},
		{"branding.source_repo.name", cfg.Branding.SourceRepo.Name, "cluster-api-provider-azure"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if cfg.CAPI == nil {
		t.Fatal("CAPI section missing")
	}
	if cfg.CAPI.ClusterNamePrefix != "capz-e2e" {
		t.Errorf("CAPI.ClusterNamePrefix = %q, want capz-e2e", cfg.CAPI.ClusterNamePrefix)
	}
	if cfg.Artifacts == nil {
		t.Fatal("Artifacts section missing")
	}
	if cfg.Artifacts.Collector != "capi" {
		t.Errorf("Artifacts.Collector = %q, want capi", cfg.Artifacts.Collector)
	}
	if got := cfg.CollectorName(); got != "capi" {
		t.Errorf("CollectorName() = %q, want capi", got)
	}
	if cfg.AI == nil {
		t.Fatal("AI section missing")
	}
	if cfg.AI.Module != "capi" {
		t.Errorf("AI.Module = %q, want capi", cfg.AI.Module)
	}
	if got := cfg.AIModuleName(); got != "capi" {
		t.Errorf("AIModuleName() = %q, want capi", got)
	}
	if cfg.AI.Endpoint != "https://api.githubcopilot.com/chat/completions" {
		t.Errorf("AI.Endpoint = %q, want copilot URL", cfg.AI.Endpoint)
	}
	if cfg.AI.Model != "claude-opus-4.6" {
		t.Errorf("AI.Model = %q, want claude-opus-4.6", cfg.AI.Model)
	}
}
