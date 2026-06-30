package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// writeFile writes content under dir, creating parents.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHandler_DataReadParity verifies that /data/* returns the fetcher output
// files byte-for-byte, including the jobs/ subdirectory.
func TestHandler_DataReadParity(t *testing.T) {
	dataDir := t.TempDir()
	files := map[string]string{
		"manifest.json":        `{"id":"demo"}`,
		"dashboard.json":       `{"jobs":[]}`,
		"flakiness.json":       `{"tests":[]}`,
		"search-index.json":    `{"entries":[]}`,
		"jobs/periodic-x.json": `{"job_id":"periodic-x"}`,
	}
	for rel, content := range files {
		writeFile(t, dataDir, rel, content)
	}

	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities()})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	for rel, want := range files {
		resp, err := http.Get(srv.URL + "/data/" + rel)
		if err != nil {
			t.Fatalf("GET %s: %v", rel, err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", rel, resp.StatusCode)
		}
		if body != want {
			t.Errorf("GET %s: body = %q, want %q", rel, body, want)
		}
	}
}

// TestHandler_Capabilities verifies the descriptor shape served in server mode.
func TestHandler_Capabilities(t *testing.T) {
	dataDir := t.TempDir()
	writeFile(t, dataDir, "manifest.json", `{}`)

	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities()})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/capabilities")
	if err != nil {
		t.Fatalf("GET capabilities: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got Capabilities
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_ = resp.Body.Close()
	if got.Mode != "server" {
		t.Errorf("Mode = %q, want server", got.Mode)
	}
	if got.Features.Chat || got.Features.Actions {
		t.Errorf("Features = %+v, want all false at read parity", got.Features)
	}
}

// TestHandler_SPAFallback verifies deep links fall back to index.html while
// real asset files are served directly.
func TestHandler_SPAFallback(t *testing.T) {
	dataDir := t.TempDir()
	writeFile(t, dataDir, "manifest.json", `{}`)
	staticDir := t.TempDir()
	writeFile(t, staticDir, "index.html", "<!doctype html><title>app</title>")
	writeFile(t, staticDir, "assets/app.js", "console.log(1)")

	h, err := Handler(Options{DataDir: dataDir, StaticDir: staticDir, Capabilities: DefaultCapabilities()})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// A real asset is served as-is.
	resp, err := http.Get(srv.URL + "/assets/app.js")
	if err != nil {
		t.Fatalf("GET asset: %v", err)
	}
	if got := readBody(t, resp); got != "console.log(1)" {
		t.Errorf("asset body = %q", got)
	}

	// A client-side route falls back to index.html.
	resp, err = http.Get(srv.URL + "/job/periodic-x/test/foo")
	if err != nil {
		t.Fatalf("GET deep link: %v", err)
	}
	if got := readBody(t, resp); got != "<!doctype html><title>app</title>" {
		t.Errorf("deep-link body = %q, want index.html", got)
	}
}

// TestHandler_NoDirectoryListing verifies /data/ does not expose a browsable
// listing of the output tree.
func TestHandler_NoDirectoryListing(t *testing.T) {
	dataDir := t.TempDir()
	writeFile(t, dataDir, "manifest.json", `{}`)
	writeFile(t, dataDir, "jobs/periodic-x.json", `{}`)

	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities()})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, p := range []string{"/data/", "/data/jobs/"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404 (no listing)", p, resp.StatusCode)
		}
	}
}

func TestHandler_MissingDataDir(t *testing.T) {
	if _, err := Handler(Options{DataDir: filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Fatal("expected error for missing data dir")
	}
	if _, err := Handler(Options{DataDir: ""}); err == nil {
		t.Fatal("expected error for empty data dir")
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}
