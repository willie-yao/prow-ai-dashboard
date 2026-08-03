package ai

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitHubRepoReaderAnonymousPublicAccess(t *testing.T) {
	var treeCalls, fileCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Fatalf("anonymous request carried Authorization")
		}
		switch {
		case strings.Contains(r.URL.Path, "/git/trees/HEAD"):
			treeCalls++
			_, _ = w.Write([]byte(`{"tree":[{"path":"config/source.yaml","type":"blob"},{"path":"config","type":"tree"}]}`))
		case r.URL.Path == "/owner/repo/HEAD/config/source.yaml":
			fileCalls++
			_, _ = w.Write([]byte("enabled: true\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldAPI, oldRaw := githubAPIBase, rawContentBase
	githubAPIBase, rawContentBase = srv.URL, srv.URL
	t.Cleanup(func() { githubAPIBase, rawContentBase = oldAPI, oldRaw })

	reader := NewGitHubRepoReader("owner", "repo", "", "")
	paths, err := reader.ListTree(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cachedPaths, err := reader.ListTree(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "config/source.yaml" || len(cachedPaths) != 1 || cachedPaths[0] != paths[0] || treeCalls != 1 {
		t.Fatalf("paths=%v cached=%v treeCalls=%d", paths, cachedPaths, treeCalls)
	}
	content, found, err := reader.ReadFile(context.Background(), "config/source.yaml")
	if err != nil || !found || content != "enabled: true\n" || fileCalls != 1 {
		t.Fatalf("content=%q found=%t fileCalls=%d err=%v", content, found, fileCalls, err)
	}
}

func TestGitHubRepoReaderAuthenticatedPrivateAccess(t *testing.T) {
	const token = "read-token-value"
	var treeAuth, fileAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/git/trees/commit-sha"):
			treeAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"tree":[{"path":"private/file.go","type":"blob"}]}`))
		case r.URL.Path == "/repos/owner/private/contents/private/file.go":
			fileAuth = r.Header.Get("Authorization")
			if r.URL.Query().Get("ref") != "commit-sha" || r.Header.Get("Accept") != "application/vnd.github.raw+json" {
				t.Fatalf("authenticated file request ref=%q accept=%q", r.URL.Query().Get("ref"), r.Header.Get("Accept"))
			}
			_, _ = w.Write([]byte("package private\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldAPI := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = oldAPI })

	reader := NewGitHubRepoReader("owner", "private", "commit-sha", token)
	if _, err := reader.ListTree(context.Background()); err != nil {
		t.Fatal(err)
	}
	content, found, err := reader.ReadFile(context.Background(), "private/file.go")
	if err != nil || !found || content != "package private\n" {
		t.Fatalf("content=%q found=%t err=%v", content, found, err)
	}
	if treeAuth != "Bearer "+token || fileAuth != "Bearer "+token {
		t.Fatalf("authorization headers tree=%q file=%q", treeAuth, fileAuth)
	}
}

func TestGitHubRepoReaderBulkAccessIsCached(t *testing.T) {
	const token = "read-token-value"
	archive := sourceArchive(t, map[string]string{
		"go.mod":     "module example/repo\n",
		"pkg/fix.go": "package fix\n",
	})
	archiveCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/private/tarball/commit-sha" {
			http.NotFound(w, r)
			return
		}
		archiveCalls++
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Fatalf("archive authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write(archive)
	}))
	defer srv.Close()
	oldAPI := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = oldAPI })

	reader := NewGitHubRepoReader("owner", "private", "commit-sha", token).(*githubRepoReader)
	for range 2 {
		files, err := reader.ReadFiles(context.Background(), []string{"go.mod", "pkg/fix.go"})
		if err != nil || files["go.mod"] != "module example/repo\n" || files["pkg/fix.go"] != "package fix\n" {
			t.Fatalf("files=%v err=%v", files, err)
		}
	}
	if archiveCalls != 1 {
		t.Fatalf("archive calls = %d, want 1", archiveCalls)
	}
}

func sourceArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	for path, content := range files {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: "owner-private-sha/" + path, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestGitHubRepoReaderRejectsTruncatedTree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"truncated":true,"tree":[{"path":"partial.go","type":"blob"}]}`))
	}))
	defer srv.Close()
	oldAPI := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = oldAPI })

	reader := NewGitHubRepoReader("owner", "repo", "commit-sha", "")
	if _, err := reader.ListTree(context.Background()); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("ListTree error = %v", err)
	}
}
