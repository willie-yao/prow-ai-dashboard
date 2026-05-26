// Package gcsweb uses the GCS JSON API to discover build IDs for Prow jobs.
package gcsweb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"unicode"
)

const (
	// GCSWebBaseURL is the base URL for GCSweb HTML listing pages.
	GCSWebBaseURL = "https://gcsweb.k8s.io/gcs/kubernetes-ci-logs/logs/"
	// GCSBaseURL is the base URL for direct GCS object access.
	GCSBaseURL = "https://storage.googleapis.com/kubernetes-ci-logs/logs/"
	// GCSListAPIURL is the GCS JSON API endpoint for listing objects in the bucket.
	GCSListAPIURL = "https://storage.googleapis.com/storage/v1/b/kubernetes-ci-logs/o"

	gcsBucket = "kubernetes-ci-logs"
	gcsPrefix = "logs/"
)

// gcsListResponse represents the JSON response from the GCS list objects API.
type gcsListResponse struct {
	Prefixes      []string `json:"prefixes"`
	NextPageToken string   `json:"nextPageToken"`
}

// ListBuildIDs uses the GCS JSON API to list all build IDs for the given job,
// sorted descending (newest first). For jobs with many builds, prefer
// ListRecentBuildIDs which is much faster.
func ListBuildIDs(ctx context.Context, client *http.Client, jobName string) ([]string, error) {
	return listAllBuildIDs(ctx, client, GCSListAPIURL, jobName)
}

// ListRecentBuildIDs returns the most recent count build IDs for the given job,
// sorted descending (newest first).
func ListRecentBuildIDs(ctx context.Context, client *http.Client, jobName string, count int) ([]string, error) {
	return listRecentBuildIDs(ctx, client, GCSListAPIURL, jobName, count)
}

func listRecentBuildIDs(ctx context.Context, client *http.Client, apiURL, jobName string, count int) ([]string, error) {
	ids, err := listAllBuildIDs(ctx, client, apiURL, jobName)
	if err != nil {
		return nil, err
	}
	if count > len(ids) {
		count = len(ids)
	}
	return ids[:count], nil
}

// listAllBuildIDs paginates through all build IDs.
func listAllBuildIDs(ctx context.Context, client *http.Client, apiURL, jobName string) ([]string, error) {
	prefix := gcsPrefix + jobName + "/"
	var allIDs []string
	pageToken := ""

	for {
		params := url.Values{
			"prefix":    {prefix},
			"delimiter": {"/"},
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
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("unexpected status %d for %s", resp.StatusCode, u)
		}
		var result gcsListResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decoding GCS response: %w", err)
		}
		resp.Body.Close()

		for _, p := range result.Prefixes {
			if id := extractBuildID(p); id != "" {
				allIDs = append(allIDs, id)
			}
		}

		if result.NextPageToken == "" {
			break
		}
		pageToken = result.NextPageToken
	}

	sort.Sort(sort.Reverse(sort.StringSlice(allIDs)))
	return allIDs, nil
}

func extractBuildID(prefix string) string {
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
