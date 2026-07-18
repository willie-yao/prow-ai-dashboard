package fetcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

func TestRunFinalizedSideEffectsLoadsFinalizedOutput(t *testing.T) {
	projectDir := t.TempDir()
	dataDir := t.TempDir()
	storageDir := t.TempDir()
	config := `
id: test
name: Test Project
testgrid:
  dashboard: test
storage:
  provider: local
  base: ` + storageDir + `
branding:
  title: Test
  base_path: /
  site_url: https://example.test
  source_repo:
    owner: example
    name: repo
`
	if err := os.WriteFile(filepath.Join(projectDir, "project.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := statefile.WriteJSON(filepath.Join(dataDir, "jobs", models.JobDataFilename("job")), models.JobDetail{JobID: "job", Name: "Job"}); err != nil {
		t.Fatal(err)
	}
	if err := statefile.WriteJSON(filepath.Join(dataDir, "flakiness.json"), models.FlakinessReport{}); err != nil {
		t.Fatal(err)
	}

	if err := RunFinalizedSideEffects(context.Background(), FinalizedSideEffectsOptions{ProjectDir: projectDir, DataDir: dataDir}); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFinalizedDataRejectsMalformedJob(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "jobs", "bad.json"), []byte(`{"job_id":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := statefile.WriteJSON(filepath.Join(dataDir, "flakiness.json"), models.FlakinessReport{}); err != nil {
		t.Fatal(err)
	}

	_, _, err := loadFinalizedData(dataDir)
	if err == nil || !strings.Contains(err.Error(), "parse finalized job") {
		t.Fatalf("error = %v, want malformed job error", err)
	}
}
