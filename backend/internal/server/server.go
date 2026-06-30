// Package server serves the dashboard over HTTP for the Kubernetes-native
// deploy mode. It is a strict superset of the static Pages contract: it serves
// the exact same /data/*.json files the SPA already reads, and adds a
// capability descriptor at /api/capabilities that lets the frontend discover
// server-only features. With no capability descriptor the frontend stays in
// read-only static mode, so one build serves both deploy targets.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Options configures a server Handler.
type Options struct {
	// DataDir is the fetcher output directory served at /data. Required.
	DataDir string
	// StaticDir is an optional built frontend (dist) served at / with SPA
	// fallback. Empty serves data and API only, with the SPA hosted elsewhere.
	StaticDir string
	// Capabilities is the descriptor returned at /api/capabilities.
	Capabilities Capabilities
}

// Capabilities tells the frontend which deploy mode it is talking to and which
// server-only features are available. Its absence (static Pages mode) means
// read-only.
type Capabilities struct {
	// Mode is "server" when served by this binary.
	Mode string `json:"mode"`
	// Features gates additive interactive UI. All false at read parity.
	Features Features `json:"features"`
}

// Features enumerates the optional interactive capabilities.
type Features struct {
	// Chat enables conversational triage UI.
	Chat bool `json:"chat"`
	// Actions enables on-page create-issue / propose-fix buttons.
	Actions bool `json:"actions"`
}

// DefaultCapabilities is the read-parity descriptor: server mode, no
// interactive features yet.
func DefaultCapabilities() Capabilities {
	return Capabilities{Mode: "server"}
}

// Handler builds the HTTP handler for the dashboard server. DataDir must exist.
func Handler(opts Options) (http.Handler, error) {
	if opts.DataDir == "" {
		return nil, fmt.Errorf("server: DataDir is required")
	}
	if info, err := os.Stat(opts.DataDir); err != nil {
		return nil, fmt.Errorf("server: data dir %q: %w", opts.DataDir, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("server: data dir %q is not a directory", opts.DataDir)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/api/capabilities", capabilitiesHandler(opts.Capabilities))

	// /data/* serves the fetcher output tree (manifest.json, dashboard.json,
	// jobs/*.json, flakiness.json, search-index.json) at read parity. Directory
	// listings are disabled so it serves files, not a browsable tree.
	dataFS := http.FileServer(noListFS{http.Dir(opts.DataDir)})
	mux.Handle("/data/", http.StripPrefix("/data/", dataFS))

	if opts.StaticDir != "" {
		if info, err := os.Stat(opts.StaticDir); err != nil {
			return nil, fmt.Errorf("server: static dir %q: %w", opts.StaticDir, err)
		} else if !info.IsDir() {
			return nil, fmt.Errorf("server: static dir %q is not a directory", opts.StaticDir)
		}
		mux.Handle("/", spaHandler(opts.StaticDir))
	}

	return mux, nil
}

// capabilitiesHandler returns the capability descriptor as JSON.
func capabilitiesHandler(c Capabilities) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c)
	}
}

// spaHandler serves a single-page app from dir: real files are served as-is,
// and any unmatched path falls back to index.html so client-side routes resolve
// on deep links and refreshes.
func spaHandler(dir string) http.HandlerFunc {
	index := filepath.Join(dir, "index.html")
	fileServer := http.FileServer(http.Dir(dir))
	return func(w http.ResponseWriter, r *http.Request) {
		clean := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		// Anything that is not a lexically local path (traversal, absolute) or
		// is the root falls back to index.html rather than touching the disk.
		if clean == "." || !filepath.IsLocal(clean) {
			http.ServeFile(w, r, index)
			return
		}
		if info, err := os.Stat(filepath.Join(dir, clean)); err == nil && !info.IsDir() {
			fileServer.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, index)
	}
}

// noListFS wraps an http.FileSystem to disable directory listings: opening a
// directory returns os.ErrNotExist, so http.FileServer responds 404 instead of
// rendering an index of the tree.
type noListFS struct{ fs http.FileSystem }

func (f noListFS) Open(name string) (http.File, error) {
	file, err := f.fs.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if info.IsDir() {
		file.Close()
		return nil, os.ErrNotExist
	}
	return file, nil
}
