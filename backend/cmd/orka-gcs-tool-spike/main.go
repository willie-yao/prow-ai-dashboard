// Command orka-gcs-tool-spike is an experimental HTTP shim for the Orka
// evaluation. It exposes the engine's real artifact tools (filesystem:
// list/read/tail/grep/find and k8s: discover_clusters/find_my_cluster/... over a
// Prow build's GCS artifact tree) as plain HTTP endpoints so Orka Tool CRDs can
// call them. It proves our existing domain code repackages as Orka-reachable
// Tools without any change to the tools themselves.
//
// TEMPORARY: this lives only on the `orka` branch. Remove it (together with
// experimental/orka/) when the Orka evaluation concludes or Orka is dropped.
package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/filesystem"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/k8s"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	bucket := env("BUCKET", "kubernetes-ci-logs")
	buildPrefix := env("BUILD_PREFIX", "logs/periodic-cluster-api-provider-azure-e2e-main/2073230525962653696/")
	display := env("DISPLAY", buildPrefix)
	addr := env("ADDR", ":8080")

	backend, err := storage.New(storage.Config{Provider: storage.ProviderGCS, Bucket: bucket}, nil)
	if err != nil {
		log.Fatalf("storage.New: %v", err)
	}
	browser := artifacts.NewBackendFactory(backend, bucket).ForBuild(buildPrefix, display)

	reg := tools.NewRegistry()
	filesystem.Register(reg)
	k8s.Register(reg)

	webURLBase := backend.WebURL(buildPrefix)

	newEnv := func() *tools.Env {
		return &tools.Env{
			Browser:             browser,
			Cache:               tools.NewCache(),
			WebURLBase:          webURLBase,
			RemainingModelBytes: 1 << 30,
			RemainingGCSBytes:   1 << 30,
		}
	}

	// POST /tool/{name}; body is the raw JSON tool arguments. Response is the
	// tool's Payload as JSON, exactly what the engine would hand the model.
	http.HandleFunc("/tool/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		name := r.URL.Path[len("/tool/"):]
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(raw) == 0 {
			raw = []byte("{}")
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		res := reg.Dispatch(ctx, newEnv(), name, json.RawMessage(raw))
		log.Printf("🛠  %s args=%s bytes=%d", name, truncate(raw, 200), res.BytesFetched)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res.Payload)
	})

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("orka-gcs-tool-spike on %s bucket=%s build=%s", addr, bucket, buildPrefix)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
