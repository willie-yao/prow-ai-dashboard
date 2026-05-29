// Package gcsweb uses the GCS JSON API to discover build IDs for Prow jobs.
package gcsweb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"unicode"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Build is a single Prow build's identity. PullNumber is the pull request
// number for presubmits and is empty for periodics.
type Build struct {
	ID         string
	PullNumber string
}

// gcsListResponse represents the JSON response from the GCS list objects API.
type gcsListResponse struct {
	Items         []gcsListItem `json:"items"`
	Prefixes      []string      `json:"prefixes"`
	NextPageToken string        `json:"nextPageToken"`
}

type gcsListItem struct {
	Name string `json:"name"`
}

// ListRecentBuilds returns up to count most-recent Builds for the given
// job, sorted descending (newest first).
//
// For periodics it walks logs/<jobName>/ as a directory listing and
// returns Build{ID: <buildID>}.
//
// For presubmits it walks pr-logs/directory/<jobName>/, which Prow keeps
// as a flat index of <buildID>.txt files spanning every PR that has ever
// triggered this job. The .txt body is the path
//
//	pr-logs/pull/<org_repo>/<pr#>/<jobName>/<buildID>
//
// Build IDs are sorted descending and each candidate is resolved
// (sequentially) by fetching its .txt body. Resolutions are strictly
// validated against the job's expected Repo + Name + buildID, so a
// same-named presubmit in a different repo cannot bleed into the result.
// Invalid candidates are skipped silently and collection continues until
// `count` valid Builds have been gathered or candidates are exhausted.
func ListRecentBuilds(ctx context.Context, client *http.Client, bucket *gcs.Bucket, job *models.ProwJob, count int) ([]Build, error) {
	if job == nil {
		return nil, fmt.Errorf("gcsweb: nil ProwJob")
	}
	if count <= 0 {
		return nil, nil
	}
	apiURL := bucket.ListAPIURL()
	switch job.JobType {
	case models.JobTypePeriodic:
		indexPrefix := bucket.JobIndexPrefix(gcs.JobLocation{JobType: job.JobType}, job.Name)
		ids, err := listPeriodicBuildIDs(ctx, client, apiURL, indexPrefix, count)
		if err != nil {
			return nil, err
		}
		out := make([]Build, len(ids))
		for i, id := range ids {
			out[i] = Build{ID: id}
		}
		return out, nil
	case models.JobTypePresubmit:
		if job.Repo == "" {
			return nil, fmt.Errorf("gcsweb: presubmit job %q requires Repo", job.Name)
		}
		indexPrefix := bucket.JobIndexPrefix(gcs.JobLocation{JobType: job.JobType}, job.Name)
		ids, err := listPresubmitCandidateIDs(ctx, client, apiURL, indexPrefix)
		if err != nil {
			return nil, err
		}
		storageBase := fmt.Sprintf("https://storage.googleapis.com/%s/", bucket.Name)
		expectedRepoSeg := strings.ReplaceAll(job.Repo, "/", "_")
		out := make([]Build, 0, count)
		for _, id := range ids {
			if len(out) >= count {
				break
			}
			pull, ok, _ := resolvePresubmitPath(ctx, client, storageBase, indexPrefix, id, expectedRepoSeg, job.Name)
			if !ok {
				continue
			}
			out = append(out, Build{ID: id, PullNumber: pull})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("gcsweb: unknown JobType %q for %q", job.JobType, job.Name)
	}
}

// listPeriodicBuildIDs paginates the periodic logs/<job>/ listing,
// stopping once max IDs have been collected.
func listPeriodicBuildIDs(ctx context.Context, client *http.Client, apiURL, prefix string, max int) ([]string, error) {
	var allIDs []string
	pageToken := ""
	for {
		page, err := fetchListPage(ctx, client, apiURL, prefix, pageToken)
		if err != nil {
			return nil, err
		}
		for _, p := range page.Prefixes {
			if id := extractBuildIDFromPrefix(p); id != "" {
				allIDs = append(allIDs, id)
			}
		}
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	sort.Sort(sort.Reverse(sort.StringSlice(allIDs)))
	if max < len(allIDs) {
		allIDs = allIDs[:max]
	}
	return allIDs, nil
}

// listPresubmitCandidateIDs paginates pr-logs/directory/<job>/ and
// returns every <buildID>.txt basename as a numeric build ID, sorted
// descending. Resolution to a (pullNumber) tuple happens later, per ID,
// against the .txt body so that cross-repo collisions can be filtered.
func listPresubmitCandidateIDs(ctx context.Context, client *http.Client, apiURL, prefix string) ([]string, error) {
	var allIDs []string
	pageToken := ""
	for {
		page, err := fetchListPage(ctx, client, apiURL, prefix, pageToken)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if id := extractBuildIDFromTxtItem(item.Name); id != "" {
				allIDs = append(allIDs, id)
			}
		}
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	sort.Sort(sort.Reverse(sort.StringSlice(allIDs)))
	return allIDs, nil
}

func fetchListPage(ctx context.Context, client *http.Client, apiURL, prefix, pageToken string) (*gcsListResponse, error) {
	params := url.Values{
		"prefix":     {prefix},
		"delimiter":  {"/"},
		"maxResults": {"1000"},
	}
	u := apiURL + "?" + params.Encode()
	if pageToken != "" {
		u += "&pageToken=" + url.QueryEscape(pageToken)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d for %s", resp.StatusCode, u)
	}
	var result gcsListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding GCS response: %w", err)
	}
	return &result, nil
}

// resolvePresubmitPath fetches pr-logs/directory/<job>/<buildID>.txt and
// returns the pull number if the body resolves to a path that matches
// the expected (repoSeg, jobName, buildID). Non-match (including
// cross-repo collisions and HTTP errors) returns ok=false with no error.
func resolvePresubmitPath(ctx context.Context, client *http.Client, storageBase, indexPrefix, buildID, expectedRepoSeg, expectedJobName string) (string, bool, error) {
	target := storageBase + indexPrefix + buildID + ".txt"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", false, err
	}
	parts := strings.Split(strings.TrimSpace(string(body)), "/")
	// Expected: pr-logs / pull / <org_repo> / <pr#> / <jobName> / <buildID>
	if len(parts) != 6 {
		return "", false, nil
	}
	if parts[0] != "pr-logs" || parts[1] != "pull" {
		return "", false, nil
	}
	if parts[2] != expectedRepoSeg || parts[4] != expectedJobName || parts[5] != buildID {
		return "", false, nil
	}
	if !isNumeric(parts[3]) {
		return "", false, nil
	}
	return parts[3], true, nil
}

// extractBuildIDFromPrefix returns the numeric directory name from a
// "<...>/<buildID>/" prefix, or "" if the trailing segment is not numeric.
func extractBuildIDFromPrefix(prefix string) string {
	prefix = strings.TrimSuffix(prefix, "/")
	idx := strings.LastIndex(prefix, "/")
	segment := prefix
	if idx >= 0 {
		segment = prefix[idx+1:]
	}
	if isNumeric(segment) {
		return segment
	}
	return ""
}

// extractBuildIDFromTxtItem returns the numeric basename of an
// "<indexPrefix><buildID>.txt" item, or "" if the basename is not
// "<digits>.txt".
func extractBuildIDFromTxtItem(name string) string {
	base := name
	if idx := strings.LastIndex(base, "/"); idx >= 0 {
		base = base[idx+1:]
	}
	if !strings.HasSuffix(base, ".txt") {
		return ""
	}
	base = strings.TrimSuffix(base, ".txt")
	if isNumeric(base) {
		return base
	}
	return ""
}

// isNumeric returns true if s is non-empty and contains only digits.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
