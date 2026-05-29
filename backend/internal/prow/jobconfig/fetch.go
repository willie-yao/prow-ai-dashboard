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

// Base URLs for fetching job configs from kubernetes/test-infra. Vars
// rather than consts so tests can swap them for an httptest.Server.
var (
	rawBaseURL = "https://raw.githubusercontent.com/kubernetes/test-infra/master/"
	apiBaseURL = "https://api.github.com/repos/kubernetes/test-infra/contents/"
)

// FetchJobConfigs discovers all of a project's Prow job YAMLs from the
// kubernetes/test-infra repository via the GitHub API, downloads them,
// and returns the parsed jobs. The set of files and the dashboard filter
// come from cfg, so new release branches or new project dashboards are
// picked up without code changes. Multiple paths may be configured; the
// union of jobs matching cfg.TestGrid.Dashboard across all paths is
// returned.
func FetchJobConfigs(ctx context.Context, client *http.Client, cfg *project.Config) ([]models.ProwJob, error) {
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

		jobs, err := ParseJobConfig(body, file, cfg.TestGrid.Dashboard, cfg.EffectiveCategories())
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", file, err)
		}
		allJobs = append(allJobs, jobs...)
	}

	if len(allJobs) == 0 {
		return nil, fmt.Errorf("no jobs labeled with dashboard %q found across %d file(s) in path(s) %v",
			cfg.TestGrid.Dashboard, len(files), cfg.Source.TestInfraPaths)
	}

	return allJobs, nil
}

// discoverConfigFiles lists every YAML config file across the project's
// configured test_infra_paths and returns repo-relative paths (not bare
// filenames) so the caller can compose unambiguous raw-download URLs
// and two files sharing the same basename across different directories
// don't collide. Files are filtered to those starting with FilePrefix
// when set; presets are always skipped.
func discoverConfigFiles(ctx context.Context, client *http.Client, cfg *project.Config) ([]string, error) {
	var allFiles []string
	seen := make(map[string]struct{})

	for _, dir := range cfg.Source.TestInfraPaths {
		files, err := listDirYAMLs(ctx, client, dir, cfg.Source.FilePrefix)
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", dir, err)
		}
		for _, f := range files {
			if _, ok := seen[f]; ok {
				continue
			}
			seen[f] = struct{}{}
			allFiles = append(allFiles, f)
		}
	}

	if len(allFiles) == 0 {
		return nil, fmt.Errorf("no config files found under %v with prefix %q",
			cfg.Source.TestInfraPaths, cfg.Source.FilePrefix)
	}

	return allFiles, nil
}

// listDirYAMLs returns repo-relative paths of every YAML file directly
// under dir (no recursion) that starts with prefix (or all when prefix
// is empty), excluding preset files.
func listDirYAMLs(ctx context.Context, client *http.Client, dir, prefix string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBaseURL+dir, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
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
		if e.Type != "file" {
			continue
		}
		if !strings.HasSuffix(e.Name, ".yaml") {
			continue
		}
		if prefix != "" && !strings.HasPrefix(e.Name, prefix) {
			continue
		}
		if strings.Contains(e.Name, "presets") {
			continue
		}
		files = append(files, dir+"/"+e.Name)
	}
	return files, nil
}
