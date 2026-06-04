package jobconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// Base URLs for talking to kubernetes/test-infra on github.com. Vars rather
// than consts so tests can swap them for an httptest.Server.
var (
	rawBaseURL    = "https://raw.githubusercontent.com/kubernetes/test-infra/master/"
	searchBaseURL = "https://api.github.com/search/code"
)

// searchPathScope restricts code-search results to the prow job tree. Anything
// outside config/jobs/ (e.g. testgrid YAMLs or doc pages that happen to mention
// a dashboard name) is not authoritative for job discovery.
const searchPathScope = "config/jobs"

// maxSearchPages caps pagination. 1000 results per query is the GitHub Search
// hard limit, so 10 pages of 100 covers the worst real-world dashboard
// comfortably and keeps a runaway scan from making 100+ HTTP calls.
const maxSearchPages = 10

// FetchJobConfigs discovers all of a project's Prow job YAMLs from the
// kubernetes/test-infra repository and returns the parsed jobs. Discovery is
// dashboard-driven: GitHub code search returns every YAML under config/jobs/
// that mentions cfg.TestGrid.Dashboard, and the testgrid-dashboards annotation
// on each parsed job is the final membership filter. False positives (a file
// mentions the dashboard string outside an annotation) are dropped by
// matchesDashboard and cost only a small wasted download.
func FetchJobConfigs(ctx context.Context, client *http.Client, cfg *project.Config) ([]models.ProwJob, error) {
	files, err := searchDashboardFiles(ctx, client, cfg.TestGrid.Dashboard)
	if err != nil {
		return nil, fmt.Errorf("searching test-infra for dashboard %q: %w", cfg.TestGrid.Dashboard, err)
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
		return nil, fmt.Errorf("no jobs labeled with dashboard %q found across %d candidate file(s) returned by code search",
			cfg.TestGrid.Dashboard, len(files))
	}

	return allJobs, nil
}

// searchResponse mirrors the parts of the GitHub Search Code API we read.
type searchResponse struct {
	TotalCount        int  `json:"total_count"`
	IncompleteResults bool `json:"incomplete_results"`
	Items             []struct {
		Path string `json:"path"`
	} `json:"items"`
}

// searchDashboardFiles asks GitHub Code Search for every YAML in
// kubernetes/test-infra/config/jobs that contains the dashboard string,
// paginates through all pages, dedupes, and returns sorted repo-relative
// paths. Requires GITHUB_TOKEN (code search is auth-only); partial or
// rate-limited responses are hard errors rather than silent under-discovery.
func searchDashboardFiles(ctx context.Context, client *http.Client, dashboard string) ([]string, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN is required for GitHub code search-based job discovery; set a token with public_repo / read access")
	}

	q := fmt.Sprintf(`"%s" repo:kubernetes/test-infra extension:yaml path:%s`, dashboard, searchPathScope)

	seen := make(map[string]struct{})
	for page := 1; page <= maxSearchPages; page++ {
		params := url.Values{}
		params.Set("q", q)
		params.Set("per_page", "100")
		params.Set("page", fmt.Sprintf("%d", page))

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchBaseURL+"?"+params.Encode(), nil)
		if err != nil {
			return nil, fmt.Errorf("creating search request (page %d): %w", page, err)
		}
		req.Header.Set("Accept", "application/vnd.github.v3+json")
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("search request (page %d): %w", page, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading search response (page %d): %w", page, err)
		}
		if resp.StatusCode != http.StatusOK {
			snippet := string(body)
			if len(snippet) > 512 {
				snippet = snippet[:512]
			}
			return nil, fmt.Errorf("search (page %d) HTTP %d: %s", page, resp.StatusCode, snippet)
		}

		var sr searchResponse
		if err := json.Unmarshal(body, &sr); err != nil {
			return nil, fmt.Errorf("parsing search response (page %d): %w", page, err)
		}
		if sr.IncompleteResults {
			return nil, fmt.Errorf("search returned incomplete_results=true on page %d; refusing to silently under-discover jobs", page)
		}

		for _, it := range sr.Items {
			if it.Path == "" {
				continue
			}
			seen[it.Path] = struct{}{}
		}

		// Stop paginating once we've collected every reported result or
		// the server stops returning items.
		if len(sr.Items) == 0 || len(seen) >= sr.TotalCount {
			break
		}
		if page == maxSearchPages && len(seen) < sr.TotalCount {
			return nil, fmt.Errorf("search returned %d results for dashboard %q, exceeds %d-page cap (%d collected)",
				sr.TotalCount, dashboard, maxSearchPages, len(seen))
		}
	}

	if len(seen) == 0 {
		return nil, fmt.Errorf("no YAML files under %s mention dashboard %q; double-check the dashboard name", searchPathScope, dashboard)
	}

	files := make([]string, 0, len(seen))
	for p := range seen {
		files = append(files, p)
	}
	sort.Strings(files)
	return files, nil
}

// DerivePeriodicPrefix returns the longest "periodic-<x>-" prefix shared by a
// strict majority of periodic jobs in the input, or "" when no such prefix
// exists. Used as a display-only hint for the frontend to strip a project's
// common boilerplate from job names. Presubmits are ignored.
func DerivePeriodicPrefix(jobs []models.ProwJob) string {
	const periodic = "periodic-"
	var names []string
	for _, j := range jobs {
		if j.JobType == models.JobTypePeriodic && strings.HasPrefix(j.Name, periodic) {
			names = append(names, j.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}

	// Count every "periodic-<tok-...->" prefix (ending at each '-' boundary)
	// across all periodic job names, then pick the longest one shared by a
	// strict majority. Stripping a longer prefix tightens display while the
	// majority threshold prevents over-stripping in dashboards with mixed
	// naming conventions.
	counts := make(map[string]int)
	for _, n := range names {
		rest := strings.TrimPrefix(n, periodic)
		for i, c := range rest {
			if c != '-' {
				continue
			}
			counts[periodic+rest[:i+1]]++
		}
	}

	threshold := len(names)/2 + 1
	var best string
	for prefix, count := range counts {
		if count < threshold {
			continue
		}
		if len(prefix) > len(best) {
			best = prefix
		}
	}
	return best
}
