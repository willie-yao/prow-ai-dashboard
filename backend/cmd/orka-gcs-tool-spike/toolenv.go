package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

// toolEnv is the per-process context handed to every self-registered quality
// tool. It exposes the build-scoped Browser plus the raw backend and a factory
// so cross-build tools (recurrence, diff_last_passing) can reach other builds.
type toolEnv struct {
	backend     storage.Backend
	bucket      string
	buildPrefix string
	webURLBase  string
	browser     artifacts.Browser
	// browserForBuild returns a Browser bound to another build in the same
	// bucket. prefix is the bucket-relative, trailing-slashed build directory.
	browserForBuild func(prefix, display string) artifacts.Browser
}

// qtool is one self-registered HTTP quality tool: an exact route plus its
// handler. Tools register themselves from an init() so adding a tool never
// requires editing main.go.
type qtool struct {
	route string
	h     func(*toolEnv, http.ResponseWriter, *http.Request)
}

var qtools []qtool

// registerQTool records a quality tool. Call it from a file-level init() in each
// tool's own file (see validate.go for the reference template).
func registerQTool(route string, h func(*toolEnv, http.ResponseWriter, *http.Request)) {
	qtools = append(qtools, qtool{route: route, h: h})
}

// requestCtx returns a per-request context with a 60s cap for tool work.
func requestCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 60*time.Second)
}

// readArgs reads and JSON-unmarshals the request body (capped at 1 MiB) into v.
// An empty body is treated as "{}". A malformed body returns an error the caller
// should surface as 400.
func readArgs(r *http.Request, v any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// writeJSON writes v as a JSON response.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// requirePOST writes 405 and returns false if the method is not POST.
func requirePOST(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return false
	}
	return true
}
