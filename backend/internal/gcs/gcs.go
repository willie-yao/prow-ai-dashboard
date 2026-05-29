// Package gcs fetches and parses Prow build artifacts (started.json, finished.json) from GCS.
package gcs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// startedJSON mirrors the JSON schema of started.json in GCS.
type startedJSON struct {
	Timestamp  int64             `json:"timestamp"`
	Repos      map[string]string `json:"repos"`
	RepoCommit string            `json:"repo-commit"`
	RepoVer    string            `json:"repo-version"`
}

// finishedJSON mirrors the JSON schema of finished.json in GCS.
type finishedJSON struct {
	Timestamp int64  `json:"timestamp"`
	Passed    bool   `json:"passed"`
	Result    string `json:"result"`
	Revision  string `json:"revision"`
}

// FetchRaw fetches the raw bytes from the given URL using the provided client and context.
// It returns a descriptive error on non-2xx status codes.
func FetchRaw(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request for %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetching %s: HTTP %d %s", url, resp.StatusCode, resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body from %s: %w", url, err)
	}
	return data, nil
}

// listObjectsResponse mirrors the GCS JSON API list response we care about:
// just the object names and the next page token.
type listObjectsResponse struct {
	Items []struct {
		Name string `json:"name"`
	} `json:"items"`
	NextPageToken string `json:"nextPageToken"`
}

// ListObjects returns every object name under the given prefix using the GCS
// JSON API, paginating through all pages. No delimiter is used, so the
// result is a flat list of full object names (not directory-like prefixes).
// The Bucket's ListAPIURL provides the endpoint.
//
// Used by collectors that walk artifact trees of unknown shape (e.g. the
// CAPI collector's controller-log discovery, which can't predict how many
// deployments or pods exist under a namespace).
func ListObjects(ctx context.Context, client *http.Client, apiURL, prefix string) ([]string, error) {
	var all []string
	pageToken := ""
	for {
		params := url.Values{
			"prefix":     {prefix},
			"maxResults": {"1000"},
		}
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}
		u := apiURL + "?" + params.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetching %s: %w", u, err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return nil, fmt.Errorf("unexpected status %d for %s", resp.StatusCode, u)
		}
		var r listObjectsResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decoding GCS response: %w", err)
		}
		resp.Body.Close()
		for _, it := range r.Items {
			all = append(all, it.Name)
		}
		if r.NextPageToken == "" {
			break
		}
		pageToken = r.NextPageToken
	}
	return all, nil
}

// queryEscape is a tiny wrapper to keep the listing helpers self-contained
// without pulling in net/url just for one call site.
func queryEscape(s string) string {
	// Only "/" needs encoding for our prefixes; everything else is ASCII
	// alphanumeric plus "-_./". Use a minimal escape.
	const hex = "0123456789ABCDEF"
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			out = append(out, c)
			continue
		}
		out = append(out, '%', hex[c>>4], hex[c&0x0f])
	}
	return string(out)
}

// fetchResult holds the result of a single artifact fetch.
type fetchResult struct {
	data []byte
	err  error
}

// FetchBuildInfo fetches started.json and finished.json for a Prow build and
// FetchBuildInfo fetches started.json and finished.json for a Prow build
// addressed by loc, and returns a populated BuildInfo. If finished.json
// is missing (HTTP 404), the build is treated as still running: partial
// info is returned with Result="PENDING" and zero Finished time.
//
// loc carries the JobType + (optional) Repo + PullNumber needed to route
// between the periodic logs/ and presubmit pr-logs/pull/ layouts via
// bucket's URL helpers.
func FetchBuildInfo(ctx context.Context, client *http.Client, bucket *Bucket, loc BuildLocation) (*models.BuildInfo, error) {
	base := bucket.BuildBaseURL(loc)
	prowURL := bucket.BuildProwURL(loc)
	webURL := bucket.BuildWebURL(loc)
	return fetchBuildInfoWithBase(ctx, client, base, prowURL, webURL, loc.JobName, loc.BuildID, loc.PullNumber)
}

// fetchBuildInfoWithBase is the internal implementation that accepts a
// fully-formed base URL (trailing-slashed prefix down to the build
// directory), making it easy to test against httptest servers.
func fetchBuildInfoWithBase(ctx context.Context, client *http.Client, base, prowURL, webURL, jobName, buildID, pullNumber string) (*models.BuildInfo, error) {
	startedURL := base + "started.json"
	finishedURL := base + "finished.json"

	// Fetch both artifacts in parallel.
	startedCh := make(chan fetchResult, 1)
	finishedCh := make(chan fetchResult, 1)

	go func() {
		data, err := FetchRaw(ctx, client, startedURL)
		startedCh <- fetchResult{data, err}
	}()
	go func() {
		data, err := FetchRaw(ctx, client, finishedURL)
		finishedCh <- fetchResult{data, err}
	}()

	startRes := <-startedCh
	finishRes := <-finishedCh

	// started.json is required.
	if startRes.err != nil {
		return nil, fmt.Errorf("fetching started.json: %w", startRes.err)
	}

	var s startedJSON
	if err := json.Unmarshal(startRes.data, &s); err != nil {
		return nil, fmt.Errorf("parsing started.json: %w", err)
	}

	info := &models.BuildInfo{
		BuildID:     buildID,
		JobName:     jobName,
		PullNumber:  pullNumber,
		WebURL:      webURL,
		Started:     time.Unix(s.Timestamp, 0).UTC(),
		Commit:      s.RepoCommit,
		RepoVersion: s.RepoVer,
		ProwURL:     prowURL,
		BuildLogURL: base + "build-log.txt",
		JUnitURL:    base + "artifacts/junit.e2e_suite.1.xml",
	}

	// finished.json may be absent if the build is still running.
	if finishRes.err != nil {
		info.Result = "PENDING"
		return info, nil
	}

	var f finishedJSON
	if err := json.Unmarshal(finishRes.data, &f); err != nil {
		return nil, fmt.Errorf("parsing finished.json: %w", err)
	}

	info.Finished = time.Unix(f.Timestamp, 0).UTC()
	info.Passed = f.Passed
	info.Result = f.Result
	info.DurationSeconds = float64(f.Timestamp - s.Timestamp)

	return info, nil
}
