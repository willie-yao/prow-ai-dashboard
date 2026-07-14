package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

// buildResolver serves any build from one process so a single shared tool
// service can back many concurrent per-build analysis Tasks. A request selects
// its build via the "build" field in the JSON body or the X-Build-Prefix header;
// absent that, defaultPrefix is used. Browsers and per-build caches are memoized.
type buildResolver struct {
	backend       storage.Backend
	factory       *artifacts.BackendFactory
	bucket        string
	defaultPrefix string

	mu       sync.Mutex
	browsers map[string]artifacts.Browser
	caches   map[string]*tools.Cache
}

func newBuildResolver(backend storage.Backend, factory *artifacts.BackendFactory, bucket, defaultPrefix string) *buildResolver {
	return &buildResolver{
		backend: backend, factory: factory, bucket: bucket,
		defaultPrefix: normalizeBuildPrefix(defaultPrefix),
		browsers:      map[string]artifacts.Browser{},
		caches:        map[string]*tools.Cache{},
	}
}

func normalizeBuildPrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// resolve returns the Browser + web-URL base + normalized prefix for a build.
func (r *buildResolver) resolve(prefix string) (artifacts.Browser, string, string) {
	prefix = normalizeBuildPrefix(prefix)
	if prefix == "" {
		prefix = r.defaultPrefix
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.browsers[prefix]
	if !ok {
		b = r.factory.ForBuild(prefix, prefix)
		r.browsers[prefix] = b
	}
	return b, r.backend.WebURL(prefix), prefix
}

func (r *buildResolver) cache(prefix string) *tools.Cache {
	prefix = normalizeBuildPrefix(prefix)
	if prefix == "" {
		prefix = r.defaultPrefix
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.caches[prefix]
	if !ok {
		c = tools.NewCache()
		r.caches[prefix] = c
	}
	return c
}

// aiEnv builds the engine-tool Env for a build.
func (r *buildResolver) aiEnv(prefix string) *tools.Env {
	b, web, p := r.resolve(prefix)
	return &tools.Env{
		Browser:             b,
		Cache:               r.cache(p),
		WebURLBase:          web,
		RemainingModelBytes: 1 << 30,
		RemainingGCSBytes:   1 << 30,
	}
}

// toolEnvFor builds the quality-tool toolEnv for a build.
func (r *buildResolver) toolEnvFor(prefix string) *toolEnv {
	b, web, p := r.resolve(prefix)
	return &toolEnv{
		backend:         r.backend,
		bucket:          r.bucket,
		buildPrefix:     p,
		webURLBase:      web,
		browser:         b,
		browserForBuild: r.factory.ForBuild,
	}
}

// requestBuild extracts the caller-selected build prefix from the X-Build-Prefix
// header (preferred) or a "build" field in the JSON body. Empty means default.
func requestBuild(r *http.Request, body []byte) string {
	if h := strings.TrimSpace(r.Header.Get("X-Build-Prefix")); h != "" {
		return h
	}
	var meta struct {
		Build string `json:"build"`
	}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &meta)
	}
	return meta.Build
}

// toolEnv is the per-request context handed to every self-registered quality
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
