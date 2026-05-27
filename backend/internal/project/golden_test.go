package project

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestLoadExampleGolden verifies that configs/example/project.yaml — the
// docs-only sample shipped with the engine — parses and validates. CAPZ-
// specific config now lives in the consumer repo, so the engine no longer
// pins CAPZ values here.
func TestLoadExampleGolden(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// backend/internal/project/golden_test.go -> ../../../configs/example/project.yaml
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "configs", "example")
	cfg, prompt, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir(%s): %v", dir, err)
	}

	checks := []struct {
		name string
		got  string
		want string
	}{
		{"id", cfg.ID, "example"},
		{"short_name", cfg.ShortName, "EXAMPLE"},
		{"testgrid.dashboard", cfg.TestGrid.Dashboard, "sig-foo-your-project"},
		{"gcs.bucket", cfg.GCS.Bucket, "kubernetes-ci-logs"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if cfg.Artifacts == nil || cfg.Artifacts.Collector != "capi" {
		t.Errorf("Artifacts.Collector wrong: %+v", cfg.Artifacts)
	}
	if cfg.AI == nil || cfg.AI.Module != "capi" {
		t.Errorf("AI.Module wrong: %+v", cfg.AI)
	}
	if prompt == "" {
		t.Error("LoadDir returned empty prompt; expected example/prompts/system.md content")
	}
}
