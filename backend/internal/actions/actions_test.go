package actions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// writeJobDetail writes a jobs/<name>.json fixture under dataDir.
func writeJobDetail(t *testing.T, dataDir, name string, detail models.JobDetail) {
	t.Helper()
	dir := filepath.Join(dataDir, "jobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func systemicPattern() models.PatternAnalysis {
	pa := models.PatternAnalysis{JobID: "periodic-x", Systemic: true, SharedRootCause: "etcd timeout"}
	pa.ID = models.PatternID(pa)
	return pa
}

func TestCreateIssue_NotFound(t *testing.T) {
	dataDir := t.TempDir()
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: "periodic-x"})
	cfg := &project.Config{Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}

	s := NewService(cfg, dataDir, AIConfig{})
	_, err := s.CreateIssue(context.Background(), "does-not-exist", "tok")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestCreateIssue_NoRepoResolved(t *testing.T) {
	dataDir := t.TempDir()
	pa := systemicPattern()
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pa}})
	// No issues repo and no branding source repo -> unresolved.
	cfg := &project.Config{}

	s := NewService(cfg, dataDir, AIConfig{})
	_, err := s.CreateIssue(context.Background(), pa.ID, "tok")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want repo-resolution error, got %v", err)
	}
}

func TestCreateIssue_NonSystemicNotActionable(t *testing.T) {
	dataDir := t.TempDir()
	pa := models.PatternAnalysis{JobID: "periodic-x", Systemic: false, SharedRootCause: "flake"}
	pa.ID = models.PatternID(pa)
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pa}})
	cfg := &project.Config{Issues: &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}

	s := NewService(cfg, dataDir, AIConfig{})
	_, err := s.CreateIssue(context.Background(), pa.ID, "tok")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want not-actionable error, got %v", err)
	}
}

func TestProposeFix_AINotConfigured(t *testing.T) {
	dataDir := t.TempDir()
	pa := systemicPattern()
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pa}})
	cfg := &project.Config{AI: &project.AI{FixPRs: &project.FixPRs{Repo: &project.SourceRepo{Owner: "o", Name: "r"}}}}

	s := NewService(cfg, dataDir, AIConfig{}) // empty AI config
	_, err := s.ProposeFix(context.Background(), pa.ID, "tok")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want AI-not-configured error, got %v", err)
	}
}
