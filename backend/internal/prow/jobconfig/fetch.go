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
)

const (
	rawBaseURL = "https://raw.githubusercontent.com/kubernetes/test-infra/master/config/jobs/kubernetes-sigs/cluster-api-provider-azure/"
	// GitHub API endpoint to list files in the CAPZ config directory.
	apiListURL = "https://api.github.com/repos/kubernetes/test-infra/contents/config/jobs/kubernetes-sigs/cluster-api-provider-azure"
	filePrefix = "cluster-api-provider-azure-"
)

// FetchJobConfigs discovers all CAPZ config YAMLs from the
// kubernetes/test-infra repository via the GitHub API, downloads them,
// and returns the parsed jobs. This automatically picks up new release
// branches or removed files without code changes.
func FetchJobConfigs(ctx context.Context, client *http.Client) ([]models.ProwJob, error) {
	files, err := discoverConfigFiles(ctx, client)
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

		jobs, err := ParseJobConfig(body, file)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", file, err)
		}
		allJobs = append(allJobs, jobs...)
	}

	return allJobs, nil
}

// discoverConfigFiles uses the GitHub Contents API to list YAML files in the
// CAPZ config directory, filtering to only CAPZ job config files (skipping
// presets and other non-job files).
func discoverConfigFiles(ctx context.Context, client *http.Client) ([]string, error) {
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
		// Only include CAPZ job YAML files (periodics + presubmits), skip presets.
		if e.Type != "file" {
			continue
		}
		if !strings.HasPrefix(e.Name, filePrefix) || !strings.HasSuffix(e.Name, ".yaml") {
			continue
		}
		if strings.Contains(e.Name, "presets") {
			continue
		}
		files = append(files, e.Name)
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no CAPZ config files found in directory listing")
	}

	return files, nil
}
