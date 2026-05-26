package jobconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

const (
	rawBaseURLTemplate = "https://raw.githubusercontent.com/kubernetes/test-infra/master/%s/"
	apiListURLTemplate = "https://api.github.com/repos/kubernetes/test-infra/contents/%s"
)

// FetchJobConfigs discovers all of a project's Prow job YAMLs from the
// kubernetes/test-infra repository via the GitHub API, downloads them,
// and returns the parsed jobs. The set of files and the dashboard filter
// come from cfg, so new release branches or new project dashboards are
// picked up without code changes.
func FetchJobConfigs(ctx context.Context, client *http.Client, cfg *project.Config) ([]models.ProwJob, error) {
	rawBaseURL := fmt.Sprintf(rawBaseURLTemplate, cfg.Source.TestInfraPath)

	files, err := discoverConfigFiles(ctx, client, cfg)
	if err != nil {
		return nil, fmt.Errorf("discovering config files: %w", err)
	}

	var allJobs []models.ProwJob
	for _, file := range files {
		url := rawBaseURL + file

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("creating request for %s: %w", file, err)
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetching %s: %w", file, err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", file, err)
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetching %s: HTTP %d", file, resp.StatusCode)
		}

		jobs, err := ParseJobConfig(body, file, cfg.TestGrid.Dashboard)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", file, err)
		}
		allJobs = append(allJobs, jobs...)
	}

	return allJobs, nil
}

// discoverConfigFiles uses the GitHub Contents API to list YAML files in the
// project's configured test-infra directory, filtering to the project's
// job config files (skipping presets and other non-job files).
func discoverConfigFiles(ctx context.Context, client *http.Client, cfg *project.Config) ([]string, error) {
	apiListURL := fmt.Sprintf(apiListURLTemplate, cfg.Source.TestInfraPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiListURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	// Use GITHUB_TOKEN for authenticated requests (higher rate limit).
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing directory: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing directory: HTTP %d", resp.StatusCode)
	}

	var entries []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("parsing directory listing: %w", err)
	}

	var files []string
	for _, e := range entries {
		// Only include the project's job YAML files (periodics + presubmits),
		// skipping presets and other shared config.
		if e.Type != "file" {
			continue
		}
		if !strings.HasPrefix(e.Name, cfg.Source.FilePrefix) || !strings.HasSuffix(e.Name, ".yaml") {
			continue
		}
		if strings.Contains(e.Name, "presets") {
			continue
		}
		files = append(files, e.Name)
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no config files found under %s with prefix %q", cfg.Source.TestInfraPath, cfg.Source.FilePrefix)
	}

	return files, nil
}
