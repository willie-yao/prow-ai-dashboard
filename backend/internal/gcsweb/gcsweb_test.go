package gcsweb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// fakeBucketServer routes both the GCS JSON listing API and raw object
// fetches against an in-memory tree. The handler dispatches on URL path:
//   - /storage/v1/b/<bucket>/o     -> JSON listing
//   - /<bucket>/<object-path>      -> raw object body
//
// The listing returns either "prefixes" (subdirectories under the prefix
// when delimiter=/ is used) or "items" (file objects directly under the
// prefix), mirroring real GCS.
type fakeBucketServer struct {
	bucket  string
	objects map[string]string // object path -> body (raw GCS object name, not URL)
}

func (f *fakeBucketServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	listingPath := "/storage/v1/b/" + f.bucket + "/o"
	if r.URL.Path == listingPath {
		f.serveListing(w, r)
		return
	}
	objectPrefix := "/" + f.bucket + "/"
	if strings.HasPrefix(r.URL.Path, objectPrefix) {
		obj := strings.TrimPrefix(r.URL.Path, objectPrefix)
		body, ok := f.objects[obj]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, body)
		return
	}
	http.NotFound(w, r)
}

func (f *fakeBucketServer) serveListing(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	delim := r.URL.Query().Get("delimiter")
	dirs := map[string]struct{}{}
	var items []gcsListItem
	for name := range f.objects {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		if delim == "/" {
			if i := strings.Index(rest, "/"); i >= 0 {
				dirs[prefix+rest[:i+1]] = struct{}{}
				continue
			}
		}
		items = append(items, gcsListItem{Name: name})
	}
	var prefixes []string
	for d := range dirs {
		prefixes = append(prefixes, d)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(gcsListResponse{Items: items, Prefixes: prefixes})
}

func newFakeBucket(t *testing.T, bucket string, objects map[string]string) (*gcs.Bucket, *http.Client) {
	t.Helper()
	srv := httptest.NewServer(&fakeBucketServer{bucket: bucket, objects: objects})
	t.Cleanup(srv.Close)
	// Wrap the client so that real-looking GCS URLs are rewritten to the
	// test server. ListAPIURL and the storage base URL both share host
	// "storage.googleapis.com" in the production helpers.
	client := &http.Client{Transport: rewriteTransport{base: srv.URL, inner: srv.Client().Transport}}
	return gcs.NewBucket(bucket), client
}

type rewriteTransport struct {
	base  string
	inner http.RoundTripper
}

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "storage.googleapis.com" {
		// Both the JSON API (/storage/v1/...) and object reads
		// (/<bucket>/...) live under the same host in production.
		req2 := req.Clone(req.Context())
		req2.URL.Scheme = "http"
		// Strip "http://" prefix from base.
		host := strings.TrimPrefix(rt.base, "http://")
		req2.URL.Host = host
		req2.Host = host
		return rt.inner.RoundTrip(req2)
	}
	return rt.inner.RoundTrip(req)
}

// TestListRecentBuilds_Periodic walks logs/<job>/ subdirectories and
// returns the most recent N build IDs with no PullNumber.
func TestListRecentBuilds_Periodic(t *testing.T) {
	objects := map[string]string{
		"logs/my-job/1111111111111111111/build-log.txt": "x",
		"logs/my-job/2222222222222222222/build-log.txt": "x",
		"logs/my-job/3333333333333333333/build-log.txt": "x",
		"logs/my-job/4444444444444444444/build-log.txt": "x",
		"logs/my-job/5555555555555555555/build-log.txt": "x",
		// Non-numeric directory should be ignored.
		"logs/my-job/latest/build-log.txt": "x",
	}
	bucket, client := newFakeBucket(t, "kubernetes-ci-logs", objects)
	job := &models.ProwJob{Name: "my-job", JobType: models.JobTypePeriodic}

	builds, err := ListRecentBuilds(context.Background(), client, bucket, job, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantIDs := []string{"5555555555555555555", "4444444444444444444", "3333333333333333333"}
	if len(builds) != len(wantIDs) {
		t.Fatalf("got %d builds, want %d", len(builds), len(wantIDs))
	}
	for i, b := range builds {
		if b.ID != wantIDs[i] {
			t.Errorf("builds[%d].ID = %q, want %q", i, b.ID, wantIDs[i])
		}
		if b.PullNumber != "" {
			t.Errorf("builds[%d].PullNumber = %q, want empty for periodic", i, b.PullNumber)
		}
	}
}

// TestListRecentBuilds_Presubmit walks pr-logs/directory/<job>/, fetches
// each .txt body, parses out the pull number, and returns Builds in
// build-id-descending order.
func TestListRecentBuilds_Presubmit(t *testing.T) {
	objects := map[string]string{
		// Index .txt files (Prow maintains these per build of every job
		// across every PR).
		"pr-logs/directory/pull-cluster-api-provider-azure-e2e/200.txt": "pr-logs/pull/kubernetes-sigs_cluster-api-provider-azure/42/pull-cluster-api-provider-azure-e2e/200",
		"pr-logs/directory/pull-cluster-api-provider-azure-e2e/100.txt": "pr-logs/pull/kubernetes-sigs_cluster-api-provider-azure/40/pull-cluster-api-provider-azure-e2e/100",
		"pr-logs/directory/pull-cluster-api-provider-azure-e2e/150.txt": "pr-logs/pull/kubernetes-sigs_cluster-api-provider-azure/41/pull-cluster-api-provider-azure-e2e/150",
		// Build artifacts under the resolved path also exist but are
		// not consulted by ListRecentBuilds; included for realism.
		"pr-logs/pull/kubernetes-sigs_cluster-api-provider-azure/42/pull-cluster-api-provider-azure-e2e/200/build-log.txt": "x",
	}
	bucket, client := newFakeBucket(t, "kubernetes-ci-logs", objects)
	job := &models.ProwJob{
		Name:    "pull-cluster-api-provider-azure-e2e",
		JobType: models.JobTypePresubmit,
		Repo:    "kubernetes-sigs/cluster-api-provider-azure",
	}

	builds, err := ListRecentBuilds(context.Background(), client, bucket, job, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []Build{
		{ID: "200", PullNumber: "42"},
		{ID: "150", PullNumber: "41"},
		{ID: "100", PullNumber: "40"},
	}
	if len(builds) != len(want) {
		t.Fatalf("got %d builds, want %d: %+v", len(builds), len(want), builds)
	}
	for i, b := range builds {
		if b != want[i] {
			t.Errorf("builds[%d] = %+v, want %+v", i, b, want[i])
		}
	}
}

// TestListRecentBuilds_PresubmitRejectsCrossRepoCollision verifies the
// strict repo+job+build filter: a same-named presubmit in a different
// repo whose .txt resolves under a different org_repo segment must NOT
// be returned.
func TestListRecentBuilds_PresubmitRejectsCrossRepoCollision(t *testing.T) {
	objects := map[string]string{
		// 300.txt resolves to the WRONG repo (looks like a different
		// project's presubmit that happens to share the job name).
		"pr-logs/directory/pull-shared-name/300.txt": "pr-logs/pull/some-other_org_repo/99/pull-shared-name/300",
		// 200.txt resolves to OUR repo (matches the strict filter).
		"pr-logs/directory/pull-shared-name/200.txt": "pr-logs/pull/kubernetes-sigs_cluster-api-provider-azure/55/pull-shared-name/200",
	}
	bucket, client := newFakeBucket(t, "kubernetes-ci-logs", objects)
	job := &models.ProwJob{
		Name:    "pull-shared-name",
		JobType: models.JobTypePresubmit,
		Repo:    "kubernetes-sigs/cluster-api-provider-azure",
	}

	builds, err := ListRecentBuilds(context.Background(), client, bucket, job, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(builds) != 1 {
		t.Fatalf("got %d builds, want 1 (cross-repo entry must be rejected): %+v", len(builds), builds)
	}
	if builds[0] != (Build{ID: "200", PullNumber: "55"}) {
		t.Errorf("builds[0] = %+v, want {ID: 200, PullNumber: 55}", builds[0])
	}
}

// TestListRecentBuilds_PresubmitRejectsBuildIDMismatch covers the case
// where the .txt body parses cleanly but the embedded buildID does not
// match the .txt filename (Prow data corruption / race). Such entries
// are dropped.
func TestListRecentBuilds_PresubmitRejectsBuildIDMismatch(t *testing.T) {
	objects := map[string]string{
		"pr-logs/directory/pull-j/100.txt": "pr-logs/pull/org_repo/1/pull-j/999",
		"pr-logs/directory/pull-j/101.txt": "pr-logs/pull/org_repo/1/pull-j/101",
	}
	bucket, client := newFakeBucket(t, "kubernetes-ci-logs", objects)
	job := &models.ProwJob{Name: "pull-j", JobType: models.JobTypePresubmit, Repo: "org/repo"}

	builds, err := ListRecentBuilds(context.Background(), client, bucket, job, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(builds) != 1 || builds[0] != (Build{ID: "101", PullNumber: "1"}) {
		t.Errorf("got %+v, want only [{ID:101 PullNumber:1}]", builds)
	}
}

// TestListRecentBuilds_PresubmitMissingRepo errors out early; presubmit
// listings require Repo to construct the strict filter.
func TestListRecentBuilds_PresubmitMissingRepo(t *testing.T) {
	bucket, client := newFakeBucket(t, "kubernetes-ci-logs", nil)
	job := &models.ProwJob{Name: "pull-x", JobType: models.JobTypePresubmit}
	_, err := ListRecentBuilds(context.Background(), client, bucket, job, 5)
	if err == nil {
		t.Fatal("expected error for presubmit with empty Repo")
	}
}

// TestListRecentBuilds_UnknownJobType errors out rather than silently
// returning empty results.
func TestListRecentBuilds_UnknownJobType(t *testing.T) {
	bucket, client := newFakeBucket(t, "kubernetes-ci-logs", nil)
	job := &models.ProwJob{Name: "x", JobType: ""}
	_, err := ListRecentBuilds(context.Background(), client, bucket, job, 5)
	if err == nil {
		t.Fatal("expected error for empty JobType")
	}
}

// TestListRecentBuilds_CountClamps caps the returned slice when fewer
// builds exist than requested.
func TestListRecentBuilds_CountClamps(t *testing.T) {
	objects := map[string]string{
		"logs/job/1/build-log.txt": "x",
		"logs/job/2/build-log.txt": "x",
	}
	bucket, client := newFakeBucket(t, "kubernetes-ci-logs", objects)
	job := &models.ProwJob{Name: "job", JobType: models.JobTypePeriodic}
	builds, err := ListRecentBuilds(context.Background(), client, bucket, job, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(builds) != 2 {
		t.Fatalf("got %d builds, want 2", len(builds))
	}
}
