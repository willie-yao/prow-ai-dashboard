package gcs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// loadFixture reads a file from the testdata directory.
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

// newTestServer creates an httptest server that serves started.json and
// finished.json under /<jobName>/<buildID>/. If serveFinished is false,
// requests for finished.json return 404.
func newTestServer(t *testing.T, jobName, buildID string, serveFinished bool) *httptest.Server {
	t.Helper()
	startedData := loadFixture(t, "started.json")
	var finishedData []byte
	if serveFinished {
		finishedData = loadFixture(t, "finished.json")
	}

	prefix := "/" + jobName + "/" + buildID + "/"
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case prefix + "started.json":
			w.Header().Set("Content-Type", "application/json")
			w.Write(startedData)
		case prefix + "finished.json":
			if !serveFinished {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(finishedData)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestFetchBuildInfo_AllFields(t *testing.T) {
	const jobName = "ci-capz-e2e"
	const buildID = "42"

	ts := newTestServer(t, jobName, buildID, true)
	defer ts.Close()

	// Construct a base URL (trailing slash) and the prow/web URLs that
	// FetchBuildInfo would otherwise derive from a *Bucket.
	base := ts.URL + "/" + jobName + "/" + buildID + "/"
	prowURL := "https://prow.example/view/" + jobName + "/" + buildID
	webURL := "https://gcsweb.example/gcs/bucket/" + jobName + "/" + buildID + "/"
	info, err := fetchBuildInfoWithBase(context.Background(), ts.Client(), base, prowURL, webURL, jobName, buildID, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify identity fields.
	assertEqual(t, "BuildID", info.BuildID, buildID)
	assertEqual(t, "JobName", info.JobName, jobName)
	assertEqual(t, "WebURL", info.WebURL, webURL)
	assertEqual(t, "PullNumber", info.PullNumber, "")

	// Verify timestamp parsing (Unix seconds → time.Time).
	wantStarted := time.Unix(1773843080, 0).UTC()
	if !info.Started.Equal(wantStarted) {
		t.Errorf("Started = %v, want %v", info.Started, wantStarted)
	}
	wantFinished := time.Unix(1773850751, 0).UTC()
	if !info.Finished.Equal(wantFinished) {
		t.Errorf("Finished = %v, want %v", info.Finished, wantFinished)
	}

	// Verify duration computation.
	wantDuration := float64(1773850751 - 1773843080)
	if info.DurationSeconds != wantDuration {
		t.Errorf("DurationSeconds = %f, want %f", info.DurationSeconds, wantDuration)
	}

	// Verify pass/result.
	if info.Passed {
		t.Error("Passed should be false")
	}
	assertEqual(t, "Result", info.Result, "FAILURE")

	// Verify commit / repo-version.
	assertEqual(t, "Commit", info.Commit, "5ad29c78143e5eee269088d91946ca2056615950")
	assertEqual(t, "RepoVersion", info.RepoVersion, "5ad29c78143e5eee269088d91946ca2056615950")

	// Verify constructed URLs.
	if !strings.HasSuffix(info.ProwURL, "/"+jobName+"/"+buildID) {
		t.Errorf("ProwURL = %s, want suffix /%s/%s", info.ProwURL, jobName, buildID)
	}
	if !strings.HasSuffix(info.BuildLogURL, "/"+jobName+"/"+buildID+"/build-log.txt") {
		t.Errorf("BuildLogURL = %s, want suffix /%s/%s/build-log.txt", info.BuildLogURL, jobName, buildID)
	}
	if len(info.JUnitURLs) != 0 {
		t.Errorf("JUnitURLs = %v, want empty (discovery now happens via DiscoverJUnitURLs, not FetchBuildInfo)", info.JUnitURLs)
	}
}

func TestFetchBuildInfo_MissingFinished(t *testing.T) {
	const jobName = "ci-capz-e2e"
	const buildID = "99"

	ts := newTestServer(t, jobName, buildID, false)
	defer ts.Close()

	base := ts.URL + "/" + jobName + "/" + buildID + "/"
	info, err := fetchBuildInfoWithBase(context.Background(), ts.Client(), base, "https://prow.example/view/"+jobName+"/"+buildID, "", jobName, buildID, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertEqual(t, "Result", info.Result, "PENDING")
	if !info.Finished.IsZero() {
		t.Errorf("Finished should be zero time for in-progress build, got %v", info.Finished)
	}
	if info.DurationSeconds != 0 {
		t.Errorf("DurationSeconds should be 0 for in-progress build, got %f", info.DurationSeconds)
	}
	// started fields should still be populated.
	assertEqual(t, "Commit", info.Commit, "5ad29c78143e5eee269088d91946ca2056615950")
}

func TestFetchBuildInfo_MissingStarted(t *testing.T) {
	// If started.json is missing, the function should return an error.
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()

	_, err := fetchBuildInfoWithBase(context.Background(), ts.Client(), ts.URL+"/job/1/", "https://prow.example/view/job/1", "", "job", "1", "")
	if err == nil {
		t.Fatal("expected error when started.json is missing")
	}
}

func TestFetchBuildInfo_ServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer ts.Close()

	_, err := fetchBuildInfoWithBase(context.Background(), ts.Client(), ts.URL+"/job/1/", "https://prow.example/view/job/1", "", "job", "1", "")
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status 500, got: %v", err)
	}
}

func TestFetchRaw_Success(t *testing.T) {
	body := `{"hello":"world"}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer ts.Close()

	data, err := FetchRaw(context.Background(), ts.Client(), ts.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != body {
		t.Errorf("got %q, want %q", string(data), body)
	}
}

func TestFetchRaw_404(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()

	_, err := FetchRaw(context.Background(), ts.Client(), ts.URL)
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention 404, got: %v", err)
	}
}

func TestFetchRaw_ContextCanceled(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := FetchRaw(ctx, ts.Client(), ts.URL)
	if err == nil {
		t.Fatal("expected error with canceled context")
	}
}

// assertEqual is a test helper for string comparisons.
func assertEqual(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", field, got, want)
	}
}

// TestListObjects_Pagination drives ListObjects through two response pages
// and verifies every object name is returned across pages, with the
// prefix correctly forwarded to the server on each call.
func TestListObjects_Pagination(t *testing.T) {
	calls := 0
	prefix := "logs/job/1/artifacts/clusters/bootstrap/logs/capi-system/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		calls++
		if got := r.URL.Query().Get("prefix"); got != prefix {
			t.Errorf("call %d: prefix = %q, want %q", calls, got, prefix)
		}
		if r.URL.Query().Get("pageToken") == "" {
			w.Write([]byte(`{"items":[{"name":"a/manager.log"},{"name":"b/manager.log"}],"nextPageToken":"tok1"}`))
		} else {
			w.Write([]byte(`{"items":[{"name":"c/manager.log"}]}`))
		}
	}))
	defer srv.Close()

	got, err := ListObjects(context.Background(), srv.Client(), srv.URL, prefix)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	want := []string{"a/manager.log", "b/manager.log", "c/manager.log"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
	if calls != 2 {
		t.Errorf("expected 2 HTTP calls, got %d", calls)
	}
}

// TestListObjects_EmptyResult verifies that an empty items array
// (the common case for missing artifact trees) returns an empty
// slice and no error.
func TestListObjects_EmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	got, err := ListObjects(context.Background(), srv.Client(), srv.URL, "anything/")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

// TestDiscoverJUnitURLs_FiltersJUnitFiles drives discoverJUnitURLsAt
// against a recorded mix of artifact files and verifies that only JUnit
// XML files (by basename pattern) are returned, with stable sort order.
func TestDiscoverJUnitURLs_FiltersJUnitFiles(t *testing.T) {
	const listPrefix = "logs/ci-foo/42/artifacts/"
	const base = "https://storage.example/bucket/logs/ci-foo/42/"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if got := r.URL.Query().Get("prefix"); got != listPrefix {
			t.Errorf("prefix = %q, want %q", got, listPrefix)
		}
		if got := r.URL.Query().Get("delimiter"); got != "/" {
			t.Errorf("delimiter = %q, want %q", got, "/")
		}
		// Simulate node-e2e + kubeadm-kinder layout in one build: three
		// junit files plus a build log, started.json, and a non-junit
		// xml that must NOT match.
		w.Write([]byte(`{"items":[
			{"name":"logs/ci-foo/42/artifacts/junit_runner.xml"},
			{"name":"logs/ci-foo/42/artifacts/junit_e2e01.xml"},
			{"name":"logs/ci-foo/42/artifacts/junit.e2e_suite.1.xml"},
			{"name":"logs/ci-foo/42/artifacts/build-log.txt"},
			{"name":"logs/ci-foo/42/artifacts/junitlike.xml"},
			{"name":"logs/ci-foo/42/artifacts/started.json"}
		]}`))
	}))
	defer srv.Close()

	got, err := discoverJUnitURLsAt(context.Background(), srv.Client(), srv.URL, base, listPrefix)
	if err != nil {
		t.Fatalf("discoverJUnitURLsAt: %v", err)
	}
	want := []string{
		base + "artifacts/junit.e2e_suite.1.xml",
		base + "artifacts/junit_e2e01.xml",
		base + "artifacts/junit_runner.xml",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestDiscoverJUnitURLs_EmptyArtifactsDir is the "build still running"
// or "no junit produced" case: list returns 200 with no matching items.
// Expect (nil, nil) so callers can cache an empty TestCases set without
// retrying.
func TestDiscoverJUnitURLs_EmptyArtifactsDir(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"items":[{"name":"logs/x/1/artifacts/build-log.txt"}]}`))
	}))
	defer srv.Close()

	got, err := discoverJUnitURLsAt(context.Background(), srv.Client(), srv.URL,
		"https://storage.example/bucket/logs/x/1/", "logs/x/1/artifacts/")
	if err != nil {
		t.Fatalf("discoverJUnitURLsAt: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

// TestDiscoverJUnitURLs_Pagination verifies nextPageToken is followed
// and junit files from every page are merged + sorted.
func TestDiscoverJUnitURLs_Pagination(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		calls++
		if r.URL.Query().Get("pageToken") == "" {
			w.Write([]byte(`{"items":[{"name":"logs/x/1/artifacts/junit_z.xml"}],"nextPageToken":"page2"}`))
		} else {
			w.Write([]byte(`{"items":[{"name":"logs/x/1/artifacts/junit_a.xml"}]}`))
		}
	}))
	defer srv.Close()

	got, err := discoverJUnitURLsAt(context.Background(), srv.Client(), srv.URL,
		"https://storage.example/bucket/logs/x/1/", "logs/x/1/artifacts/")
	if err != nil {
		t.Fatalf("discoverJUnitURLsAt: %v", err)
	}
	want := []string{
		"https://storage.example/bucket/logs/x/1/artifacts/junit_a.xml",
		"https://storage.example/bucket/logs/x/1/artifacts/junit_z.xml",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
	if calls != 2 {
		t.Errorf("expected 2 HTTP calls, got %d", calls)
	}
}

// TestDiscoverJUnitURLs_ServerError returns an error to the caller so
// fetchBuildResult can log it and fall back to caching an empty
// TestCases slice without misinterpreting the failure as "no junit".
func TestDiscoverJUnitURLs_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := discoverJUnitURLsAt(context.Background(), srv.Client(), srv.URL,
		"https://storage.example/bucket/logs/x/1/", "logs/x/1/artifacts/")
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status 500, got: %v", err)
	}
}
