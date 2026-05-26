// Package gcs fetches and parses Prow build artifacts (started.json, finished.json) from GCS.
package gcs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

const (
	// GCSBaseURL is the public GCS URL prefix for Prow build logs.
	GCSBaseURL = "https://storage.googleapis.com/kubernetes-ci-logs/logs/"
	// ProwBaseURL is the Prow UI URL prefix for viewing build logs.
	ProwBaseURL = "https://prow.k8s.io/view/gs/kubernetes-ci-logs/logs/"
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

// fetchResult holds the result of a single artifact fetch.
type fetchResult struct {
	data []byte
	err  error
}

// FetchBuildInfo fetches started.json and finished.json for a Prow build and
// returns a populated BuildInfo. If finished.json is missing (HTTP 404), the
// build is treated as still running: partial info is returned with Result set
// to "PENDING" and a zero Finished time.
func FetchBuildInfo(ctx context.Context, client *http.Client, jobName, buildID string) (*models.BuildInfo, error) {
	return fetchBuildInfoWithBase(ctx, client, GCSBaseURL, jobName, buildID)
}

// fetchBuildInfoWithBase is the internal implementation that accepts a
// configurable base URL, making it easy to test against httptest servers.
func fetchBuildInfoWithBase(ctx context.Context, client *http.Client, gcsBase, jobName, buildID string) (*models.BuildInfo, error) {
	base := gcsBase + jobName + "/" + buildID + "/"
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

	prowURL := ProwBaseURL + jobName + "/" + buildID
	info := &models.BuildInfo{
		BuildID:     buildID,
		JobName:     jobName,
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
