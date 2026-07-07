// Command server serves the dashboard's pre-computed JSON over HTTP for the
// Kubernetes-native deploy mode. It serves the same /data/*.json contract the
// static Pages site reads, plus /api/capabilities so the frontend can light up
// server-only features. The static Pages mode keeps working unchanged.
//
// When -project-dir is set and ADMIN_LOGINS names at least one GitHub login,
// the admin-gated write endpoints (create-issue, propose-fix) are enabled.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actions"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/auth"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/server"
)

func main() {
	var (
		addr       string
		dataDir    string
		staticDir  string
		projectDir string
	)
	flag.StringVar(&addr, "addr", ":8080", "listen address")
	flag.StringVar(&dataDir, "data-dir", "data", "directory of fetcher JSON output served at /data")
	flag.StringVar(&staticDir, "static-dir", "", "optional built frontend (dist) served at / with SPA fallback")
	flag.StringVar(&projectDir, "project-dir", "", "project.yaml directory; enables admin actions when set with ADMIN_LOGINS")
	flag.Parse()

	opts := server.Options{
		DataDir:      dataDir,
		StaticDir:    staticDir,
		Capabilities: server.DefaultCapabilities(),
	}

	// Enable admin-gated actions only when a project config and an admin
	// allowlist are both provided. Otherwise the server stays read-only.
	admins := splitList(os.Getenv("ADMIN_LOGINS"))
	if projectDir != "" && len(admins) > 0 {
		a, act, err := buildActions(projectDir, dataDir, admins)
		if err != nil {
			log.Fatalf("server: enabling actions: %v", err)
		}
		opts.Auth = a
		opts.Actions = act
		log.Printf("🔐 admin actions enabled for %d login(s)", len(admins))
	} else {
		log.Println("actions disabled (set -project-dir and ADMIN_LOGINS to enable)")
	}

	handler, err := server.Handler(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Shut down gracefully on SIGINT/SIGTERM so K8s rollouts drain cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("🌐 serving %s -> data=%s static=%q", addr, dataDir, staticDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("server: graceful shutdown: %v", err)
	}
}

// buildActions loads the project config and wires the PAT authenticator and the
// action service. AI credentials come from the AI_* env for fix-PR drafting.
func buildActions(projectDir, dataDir string, admins []string) (auth.Authenticator, *actions.Service, error) {
	cfg, err := project.Load(filepath.Join(projectDir, "project.yaml"))
	if err != nil {
		return nil, nil, fmt.Errorf("loading project config: %w", err)
	}
	aiCfg := actions.AIConfig{
		Token:    os.Getenv("AI_TOKEN"),
		Endpoint: firstNonEmpty(aiField(cfg, "endpoint"), os.Getenv("AI_ENDPOINT")),
		Model:    firstNonEmpty(aiField(cfg, "model"), os.Getenv("AI_MODEL")),
		Headers:  aiHeaders(cfg),
	}
	return auth.NewPATAuthenticator(admins), actions.NewService(cfg, dataDir, aiCfg), nil
}

// aiField returns cfg.AI.<endpoint|model> or "" when AI is unset.
func aiField(cfg *project.Config, which string) string {
	if cfg.AI == nil {
		return ""
	}
	switch which {
	case "endpoint":
		return cfg.AI.Endpoint
	case "model":
		return cfg.AI.Model
	}
	return ""
}

func aiHeaders(cfg *project.Config) map[string]string {
	if cfg.AI == nil || len(cfg.AI.Headers) == 0 {
		return nil
	}
	return cfg.AI.Headers
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// splitList parses a comma or whitespace separated list, dropping blanks.
func splitList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	var out []string
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
