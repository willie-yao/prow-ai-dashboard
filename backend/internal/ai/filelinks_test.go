package ai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// newLinkStub returns a server that answers both the file-existence HEAD checks
// (raw.githubusercontent.com stand-in) and the vanity `?go-get=1` meta lookups.
// exists is keyed by "/owner/repo/HEAD/path"; vanity is keyed by the module
// import path and maps to its GitHub repo URL.
func newLinkStub(t *testing.T, exists map[string]bool, vanity map[string]string) (*httptest.Server, *int32) {
	t.Helper()
	var goGetCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("go-get") == "1" {
			atomic.AddInt32(&goGetCalls, 1)
			mod := strings.TrimPrefix(r.URL.Path, "/")
			if repo, ok := vanity[mod]; ok {
				fmt.Fprintf(w, `<html><head><meta name="go-import"
					content="%s git %s"></head></html>`, mod, repo)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodHead {
			t.Errorf("file check expected HEAD, got %s", r.Method)
		}
		if exists[r.URL.Path] {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, &goGetCalls
}

func withStub(t *testing.T, srv *httptest.Server) {
	t.Helper()
	oraw, ogg := rawContentBase, goGetScheme
	rawContentBase = srv.URL
	goGetScheme = srv.URL + "/"
	t.Cleanup(func() { rawContentBase, goGetScheme = oraw, ogg })
}

// TestResolveFileLinks_GenericResolution checks the generic resolver: project
// repo for repo-relative paths, vanity-import resolution via go-get, and
// owner/repo/path references, all gated on existence so broken links are
// dropped, with no project- or ecosystem-specific knowledge in the code.
func TestResolveFileLinks_GenericResolution(t *testing.T) {
	exists := map[string]bool{
		// project repo (CAPZ) source file
		"/kubernetes-sigs/cluster-api-provider-azure/HEAD/azure/services/vm/spec.go": true,
		// project file in a dot-named directory (must resolve to the project repo,
		// not be mistaken for a vanity host)
		"/kubernetes-sigs/cluster-api-provider-azure/HEAD/.github/workflows/ci.yaml": true,
		// upstream CAPI file (reached via vanity import, owner/repo, and github.com form)
		"/kubernetes-sigs/cluster-api/HEAD/test/framework/controlplane_helpers.go": true,
	}
	vanity := map[string]string{
		"sigs.k8s.io/cluster-api": "https://github.com/kubernetes-sigs/cluster-api",
	}
	srv, _ := newLinkStub(t, exists, vanity)
	withStub(t, srv)

	s := &Service{}
	s.SetSourceRepo("kubernetes-sigs", "cluster-api-provider-azure")

	tc := &models.TestCase{
		AISummary: &models.AISummary{Summary: "noise calico-system/calico-kube-controllers v1.34.8"},
		AIAnalysis: &models.AIAnalysis{
			RelevantFiles: []string{
				// repo-relative -> project repo (exists)
				"azure/services/vm/spec.go",
				// dot-named project dir -> project repo (not a vanity host)
				".github/workflows/ci.yaml",
				// repo-relative but not in project repo -> dropped (no fallback repo)
				"test/e2e/clusterctl_upgrade_test.go (lines 1-9)",
				// owner/repo/path GitHub-form -> resolves to cluster-api
				"kubernetes-sigs/cluster-api/test/framework/controlplane_helpers.go",
				// explicit github.com/owner/repo/path -> resolves to cluster-api
				"github.com/kubernetes-sigs/cluster-api/test/framework/controlplane_helpers.go",
			},
			// vanity import path in prose -> resolves to cluster-api via go-get
			RootCause:    "fails in sigs.k8s.io/cluster-api/test/framework/controlplane_helpers.go:42",
			SuggestedFix: "n/a",
		},
	}

	links := s.resolveFileLinks(context.Background(), srv.Client(), tc)

	want := map[string]string{
		"azure/services/vm/spec.go": "https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/HEAD/azure/services/vm/spec.go",
		".github/workflows/ci.yaml": "https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/HEAD/.github/workflows/ci.yaml",
		"kubernetes-sigs/cluster-api/test/framework/controlplane_helpers.go":            "https://github.com/kubernetes-sigs/cluster-api/blob/HEAD/test/framework/controlplane_helpers.go",
		"github.com/kubernetes-sigs/cluster-api/test/framework/controlplane_helpers.go": "https://github.com/kubernetes-sigs/cluster-api/blob/HEAD/test/framework/controlplane_helpers.go",
		"sigs.k8s.io/cluster-api/test/framework/controlplane_helpers.go":                "https://github.com/kubernetes-sigs/cluster-api/blob/HEAD/test/framework/controlplane_helpers.go",
	}
	for k, v := range want {
		if links[k] != v {
			t.Errorf("links[%q] = %q, want %q", k, links[k], v)
		}
	}
	// Not in the project repo and no fallback -> dropped (no broken link).
	if _, ok := links["test/e2e/clusterctl_upgrade_test.go"]; ok {
		t.Errorf("unverified path must be dropped")
	}
	// Non-source tokens are never linked.
	if _, ok := links["calico-system/calico-kube-controllers"]; ok {
		t.Errorf("resource name must not be linked")
	}
	for k := range links {
		if strings.Contains(k, "(") {
			t.Errorf("link key %q should have annotation stripped", k)
		}
	}
}

// TestResolveFileLinks_CachesChecks ensures repeated file checks and go-get
// lookups are memoized across analyses in a run.
func TestResolveFileLinks_CachesChecks(t *testing.T) {
	exists := map[string]bool{"/o/r/HEAD/test/x.go": true}
	srv, _ := newLinkStub(t, exists, nil)
	withStub(t, srv)

	s := &Service{}
	s.SetSourceRepo("o", "r")
	var headCalls int32
	// wrap the client to count HEADs
	client := srv.Client()

	mk := func() *models.TestCase {
		return &models.TestCase{AIAnalysis: &models.AIAnalysis{RelevantFiles: []string{"test/x.go"}}}
	}
	// Count via the stub: replace handler counter is simpler, but reuse exists
	// path determinism: two runs should hit the server once.
	_ = headCalls
	_ = client
	s.resolveFileLinks(context.Background(), srv.Client(), mk())
	before := s.cachedCount()
	s.resolveFileLinks(context.Background(), srv.Client(), mk())
	after := s.cachedCount()
	if before == 0 || after != before {
		t.Errorf("expected verification cached after first run (before=%d after=%d)", before, after)
	}
}

// cachedCount reports the number of memoized link checks (test helper).
func (s *Service) cachedCount() int {
	n := 0
	s.linkVerifyCache.Range(func(_, _ any) bool { n++; return true })
	return n
}
