package eval

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/modules/universal"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/filesystem"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/k8s"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

// Config configures a Runner. It mirrors the production analysis wiring so eval
// scores represent the engine as shipped: the same prompt, agentic options
// (floors, critique, evidence injection, tools), and skill set.
type Config struct {
	// ArtifactRoot is the local directory the analyzer reads build artifacts
	// from (the dataset's artifacts tree).
	ArtifactRoot string
	// SystemPrompt is the composed system prompt (use ai.ComposeSystemPrompt).
	SystemPrompt string
	// Connect carries the endpoint/model/token/headers; CacheDir is ignored, the
	// runner supplies a throwaway cache per run.
	Connect ai.Options
	// Opts is the resolved agentic tuning (floors, critique, budgets, timeout).
	Opts ai.AgenticOptions
	// EnabledTools defaults to filesystem+k8s.
	EnabledTools []string
	// Skills feeds the critique gate, exactly as in production. nil disables it.
	Skills *skills.Set
}

// Runner executes the real agentic analysis (the same Service.Analyze path
// production uses) over a dataset's recorded artifacts. Each run uses a fresh,
// throwaway AI cache so every case (and every repeated sample of a case) is a
// real model call: the eval must measure model behavior, not cache hits.
type Runner struct {
	cfg          Config
	registry     *tools.Registry
	enabledTools []string
}

// NewRunner builds a Runner from cfg.
func NewRunner(cfg Config) (*Runner, error) {
	if cfg.ArtifactRoot == "" {
		return nil, fmt.Errorf("eval: artifact root is required")
	}
	if cfg.Connect.Endpoint == "" || cfg.Connect.Model == "" {
		return nil, fmt.Errorf("eval: connection endpoint and model are required")
	}
	registry := tools.NewRegistry()
	filesystem.Register(registry)
	k8s.Register(registry)
	toolNames := cfg.EnabledTools
	if len(toolNames) == 0 {
		toolNames = []string{"filesystem", "k8s"}
	}
	enabled, err := registry.Enable(toolNames)
	if err != nil {
		return nil, fmt.Errorf("eval: enabling tools: %w", err)
	}
	return &Runner{cfg: cfg, registry: registry, enabledTools: enabled}, nil
}

// EnabledTools returns the canonical tool set resolved by registry.Enable, so
// callers can fingerprint the actual investigation capability (default and an
// explicit filesystem+k8s normalize to the same set).
func (r *Runner) EnabledTools() []string { return r.enabledTools }

// Run analyzes one case once and returns the produced summary/analysis plus the
// build's artifact file list (for citation scoring).
func (r *Runner) Run(ctx context.Context, c Case) (*models.AISummary, *models.AIAnalysis, []string, error) {
	backend, err := storage.NewLocalBackend(r.cfg.ArtifactRoot, "")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("eval: artifact backend: %w", err)
	}
	// A throwaway cache per run guarantees a fresh analysis (no cross-case or
	// cross-sample cache hits), which is required to measure model behavior.
	cacheDir, err := os.MkdirTemp("", "eval-cache-")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("eval: cache dir: %w", err)
	}
	defer os.RemoveAll(cacheDir)
	client := ai.NewClientWithOptions(ai.Options{
		Endpoint:     r.cfg.Connect.Endpoint,
		Model:        r.cfg.Connect.Model,
		Token:        r.cfg.Connect.Token,
		ExtraHeaders: r.cfg.Connect.ExtraHeaders,
		CacheDir:     cacheDir,
	})

	factory := artifacts.NewBackendFactory(backend, "eval")
	svc := ai.NewService(client, universal.New(), r.cfg.SystemPrompt, nil)
	svc.EnableAgentic(r.cfg.Opts, factory, r.registry, r.enabledTools)
	// Skills feed the critique gate; load them so the eval reflects production.
	// Source-repo file-link verification is intentionally not set: it only
	// populates UI links via GitHub calls and does not affect analysis quality.
	svc.SetSkills(r.cfg.Skills)

	run := &models.BuildResult{BuildInfo: models.BuildInfo{JobName: c.Job, BuildID: c.Build}}
	tc := &models.TestCase{Name: c.TestName, FailureMessage: c.FailureMessage, Status: "failed"}
	svc.Analyze(ctx, &http.Client{}, c.Job, c.BuildPrefix, run, tc)

	files, _, err := backend.ListTree(ctx, c.BuildPrefix, 5000)
	if err != nil {
		return tc.AISummary, tc.AIAnalysis, nil, nil
	}
	return tc.AISummary, tc.AIAnalysis, files, nil
}

// ScoreCase runs a case and scores the result against its labels.
func (r *Runner) ScoreCase(ctx context.Context, c Case) (CaseScore, error) {
	sum, an, files, err := r.Run(ctx, c)
	if err != nil {
		return CaseScore{Name: c.Name}, err
	}
	return Score(c, sum, an, ArtifactFileSet(files)), nil
}

// RunDataset scores every case and aggregates the results.
func (r *Runner) RunDataset(ctx context.Context, cases []Case) (Scorecard, error) {
	scores := make([]CaseScore, 0, len(cases))
	for _, c := range cases {
		s, err := r.ScoreCase(ctx, c)
		if err != nil {
			return Scorecard{}, fmt.Errorf("eval: case %s: %w", c.Name, err)
		}
		scores = append(scores, s)
	}
	SortScores(scores)
	return Aggregate(scores), nil
}
