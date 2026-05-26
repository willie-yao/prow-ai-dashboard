// Package output writes pre-processed JSON files for the React frontend.
package output

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9\-_]`)

// SanitizeFilename replaces characters that are not alphanumeric, '-', or '_' with '-'.
func SanitizeFilename(name string) string {
	return unsafeChars.ReplaceAllString(name, "-")
}

// writeJSON marshals v as indented JSON and writes it to path, creating parent dirs as needed.
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// WriteDashboard writes dashboard.json to dir.
func WriteDashboard(dir string, dashboard models.Dashboard) error {
	return writeJSON(filepath.Join(dir, "dashboard.json"), dashboard)
}

// WriteJobDetail writes a per-job detail file to dir/jobs/{sanitized-name}.json.
func WriteJobDetail(dir string, detail models.JobDetail) error {
	name := SanitizeFilename(detail.Name) + ".json"
	return writeJSON(filepath.Join(dir, "jobs", name), detail)
}

// WriteFlakinessReport writes flakiness.json to dir.
func WriteFlakinessReport(dir string, report models.FlakinessReport) error {
	return writeJSON(filepath.Join(dir, "flakiness.json"), report)
}

// WriteSearchIndex writes search-index.json to dir.
func WriteSearchIndex(dir string, index models.SearchIndex) error {
	return writeJSON(filepath.Join(dir, "search-index.json"), index)
}

// WriteAll writes dashboard.json, all job detail files, flakiness.json, and search-index.json.
// Returns the first error encountered.
func WriteAll(dir string, dashboard models.Dashboard, details []models.JobDetail, flakiness models.FlakinessReport, searchIndex models.SearchIndex) error {
	if err := WriteDashboard(dir, dashboard); err != nil {
		return err
	}
	for _, d := range details {
		if err := WriteJobDetail(dir, d); err != nil {
			return err
		}
	}
	if err := WriteFlakinessReport(dir, flakiness); err != nil {
		return err
	}
	if err := WriteSearchIndex(dir, searchIndex); err != nil {
		return err
	}
	return nil
}
