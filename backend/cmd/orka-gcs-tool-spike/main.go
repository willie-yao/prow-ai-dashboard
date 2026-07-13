// Command orka-gcs-tool-spike is a throwaway HTTP shim for the Orka evaluation
// spike (Q2). It exposes the engine's real filesystem tools (list/read/tail/
// grep/find over a Prow build's GCS artifact tree) as plain HTTP endpoints so
// an Orka Tool CRD can call them. It proves our existing domain code repackages
// as an Orka-reachable Tool without any change to the tools themselves.
//
// Not part of any deploy; do not commit.
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

	newEnv := func() *tools.Env {
		return &tools.Env{
			Browser:             browser,
			Cache:               tools.NewCache(),
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
