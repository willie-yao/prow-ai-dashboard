// Command eval runs the AI failure-analysis quality evaluation over a labeled
// dataset and writes a scorecard. It is an offline, on-demand harness: model
// output is non-deterministic, so the scorecard is a tracked measurement and
// supports A/B comparison across models, prompts, or config, not a CI gate.
//
// Connection comes from the AI_ENDPOINT, AI_MODEL, and AI_TOKEN env vars (the
// same as the fetcher). Pass -project-dir to load the real prompt, skills, and
// agentic config so the eval mirrors production; vary the model/prompt/config
// and compare with -baseline.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/eval"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// fallbackModelByteBudget mirrors the fetcher's static model budget used when
// the endpoint's context window cannot be detected. The eval keeps it static so
// runs are deterministic and comparable.
const fallbackModelByteBudget = 300_000

func main() {
	dataset := flag.String("dataset", "", "dataset directory containing cases.json and artifacts/")
	projectDir := flag.String("project-dir", "", "project dir to load real prompt/skills/agentic config from (production fidelity)")
	outDir := flag.String("out", "eval/out", "output directory for scorecard.json and summary.md")
	samples := flag.Int("samples", 1, "analyze the dataset this many times to measure run-to-run variance")
	baseline := flag.String("baseline", "", "optional baseline scorecard.json to A/B against")
	title := flag.String("title", "eval", "title for the scorecard")
	maxIters := flag.Int("max-iters", 15, "agentic max iterations per case (override project config)")
	minTools := flag.Int("min-tools", 0, "agentic min tool calls floor (override project config)")
	minGCS := flag.Int("min-gcs-bytes", 0, "agentic min GCS bytes floor (override project config)")
	modelBytes := flag.Int("model-bytes", fallbackModelByteBudget, "agentic model byte budget")
	timeout := flag.Duration("timeout", 5*time.Minute, "per-case analysis timeout (override project config)")
	flag.Parse()

	if *dataset == "" {
		log.Fatal("eval: -dataset is required")
	}
	if *samples < 1 {
		log.Fatal("eval: -samples must be >= 1")
	}
	if *maxIters < 1 {
		log.Fatal("eval: -max-iters must be >= 1")
	}
	if *minTools < 0 || *minGCS < 0 || *modelBytes < 1 {
		log.Fatal("eval: -min-tools/-min-gcs-bytes must be >= 0 and -model-bytes >= 1")
	}
	endpoint, model, token := os.Getenv("AI_ENDPOINT"), os.Getenv("AI_MODEL"), os.Getenv("AI_TOKEN")
	if endpoint == "" || model == "" {
		log.Fatal("eval: AI_ENDPOINT and AI_MODEL must be set")
	}
	set := setFlags()

	ds, err := eval.LoadDataset(*dataset)
	if err != nil {
		log.Fatalf("%v", err)
	}
	log.Printf("eval: %d case(s) from %s; model=%s endpoint=%s", len(ds.Cases), *dataset, model, endpoint)

	// Default prompt/agentic config; -project-dir overrides them with the real
	// production wiring so the eval measures the engine as shipped.
	systemPrompt := "You are a CI failure analyst."
	opts := ai.AgenticOptions{
		MaxIters:        *maxIters,
		ModelByteBudget: *modelBytes,
		GCSByteBudget:   1_000_000_000,
		Timeout:         *timeout,
		MinToolCalls:    *minTools,
		MinGCSBytes:     *minGCS,
	}
	var skillSet *skills.Set
	var toolNames []string
	if *projectDir != "" {
		cfg, addendum, err := project.LoadDir(*projectDir)
		if err != nil {
			log.Fatalf("eval: loading project %s: %v", *projectDir, err)
		}
		systemPrompt = ai.ComposeSystemPrompt(addendum)
		skillSet, err = skills.Load(*projectDir)
		if err != nil {
			log.Fatalf("eval: loading skills from %s: %v", *projectDir, err)
		}
		opts, toolNames = agenticFromProject(cfg, skillSet, opts, set)
	}

	runner, err := eval.NewRunner(eval.Config{
		ArtifactRoot: ds.ArtifactRoot,
		SystemPrompt: systemPrompt,
		Connect:      ai.Options{Endpoint: endpoint, Model: model, Token: token},
		Opts:         opts,
		EnabledTools: toolNames,
		Skills:       skillSet,
	})
	if err != nil {
		log.Fatalf("eval: %v", err)
	}

	meta := eval.Meta{
		DatasetFingerprint: ds.Fingerprint(),
		Model:              model,
		PromptFingerprint:  ai.PromptFingerprint(systemPrompt),
		AgenticFingerprint: agenticFingerprint(opts, runner.EnabledTools(), skillSet),
		MaxIters:           opts.MaxIters,
		MinToolCalls:       opts.MinToolCalls,
		MinGCSBytes:        opts.MinGCSBytes,
		Critique:           opts.CritiqueEnabled,
		Samples:            *samples,
	}

	ctx := context.Background()
	cards := make([]eval.Scorecard, 0, *samples)
	for i := 0; i < *samples; i++ {
		sc, err := runner.RunDataset(ctx, ds.Cases)
		if err != nil {
			log.Fatalf("eval: %v", err)
		}
		sc.Meta = meta
		cards = append(cards, sc)
		log.Printf("eval: sample %d/%d — accuracy=%.2f real-bug F1=%.2f grounding=%.2f coverage=%.2f",
			i+1, *samples, sc.Accuracy, sc.F1, sc.GroundingRate, sc.Coverage)
	}

	// For N>1 the persisted scorecard is the mean across samples, so the central
	// tendency (not one arbitrary run) is the tracked measurement and A/B basis.
	final := eval.MeanScorecards(cards)
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("eval: %v", err)
	}
	writeJSON(filepath.Join(*outDir, "scorecard.json"), final)

	summary := eval.RenderMarkdown(*title, final)
	if *samples > 1 {
		summary += "\n" + renderVariance(cards)
	}
	if *baseline != "" {
		base, err := readScorecard(*baseline)
		if err != nil {
			log.Fatalf("eval: reading baseline: %v", err)
		}
		summary += "\n" + eval.RenderDiffMarkdown("baseline", *title, base, final)
	}
	writeFile(filepath.Join(*outDir, "summary.md"), summary)
	fmt.Print(summary)
	log.Printf("eval: wrote %s/scorecard.json and summary.md", *outDir)
}

// agenticFingerprint hashes the resolved agentic tuning, the canonical enabled
// tool set, and the skill set so an A/B comparison can detect that the two sides
// ran under different config (a confound the diff would otherwise hide). The
// model and prompt are fingerprinted separately. Tool names are sorted so order
// and default-vs-explicit selections normalize to the same hash.
func agenticFingerprint(o ai.AgenticOptions, enabledTools []string, skillSet *skills.Set) string {
	h := sha256.New()
	fmt.Fprintf(h, "iters=%d model_bytes=%d gcs_bytes=%d ctx_bytes=%d timeout=%s min_tools=%d min_gcs=%d critique=%v retries=%d single=%v evidence=%v",
		o.MaxIters, o.ModelByteBudget, o.GCSByteBudget, o.ContextByteBudget, o.Timeout, o.MinToolCalls, o.MinGCSBytes,
		o.CritiqueEnabled, o.CritiqueMaxRetries, o.SingleToolCall, o.EvidenceInjection)
	tools := append([]string(nil), enabledTools...)
	sort.Strings(tools)
	fmt.Fprintf(h, " tools=%v", tools)
	if skillSet != nil {
		fmt.Fprintf(h, " skills=%s", skillSet.Hash())
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// setFlags returns the set of flags the user passed explicitly, so project
// config can supply defaults while explicit flags still override them.
func setFlags() map[string]bool {
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// agenticFromProject mirrors the fetcher's EffectiveAgentic -> AgenticOptions
// mapping so the eval reflects production: floors, critique, evidence injection,
// and tools come from project.yaml. Skills auto-enable the critique gate they
// feed (as in fetcher). Explicit CLI flags still override the project values.
// Returns the resolved options and the enabled tool names.
func agenticFromProject(cfg *project.Config, skillSet *skills.Set, base ai.AgenticOptions, set map[string]bool) (ai.AgenticOptions, []string) {
	eff := cfg.AI.EffectiveAgentic()
	if !eff.Critique.Enabled && skillSet != nil && len(skillSet.Skills()) > 0 {
		eff.Critique.Enabled = true
	}
	out := ai.AgenticOptions{
		MaxIters:           eff.MaxIters,
		ModelByteBudget:    base.ModelByteBudget,
		GCSByteBudget:      base.GCSByteBudget,
		Timeout:            eff.Timeout,
		MinToolCalls:       eff.MinToolCalls,
		MinGCSBytes:        eff.MinGCSBytes,
		CritiqueEnabled:    eff.Critique.Enabled,
		CritiqueMaxRetries: eff.Critique.MaxRetries,
		SingleToolCall:     eff.SingleToolCall,
		EvidenceInjection:  eff.EvidenceInjection,
	}
	if set["max-iters"] {
		out.MaxIters = base.MaxIters
	}
	if set["min-tools"] {
		out.MinToolCalls = base.MinToolCalls
	}
	if set["min-gcs-bytes"] {
		out.MinGCSBytes = base.MinGCSBytes
	}
	if set["timeout"] {
		out.Timeout = base.Timeout
	}
	if set["model-bytes"] {
		out.ModelByteBudget = base.ModelByteBudget
	}
	return out, eff.Tools
}

// renderVariance reports the spread of key metrics across samples, since the
// model is non-deterministic and a single sample can mislead.
func renderVariance(cards []eval.Scorecard) string {
	f1 := make([]float64, len(cards))
	acc := make([]float64, len(cards))
	for i, c := range cards {
		f1[i], acc[i] = c.F1, c.Accuracy
	}
	f1Min, f1Max, f1Mean := stats(f1)
	accMin, accMax, accMean := stats(acc)
	return fmt.Sprintf("## Variance over %d samples\n\n| Metric | mean | min | max |\n|---|---|---|---|\n"+
		"| Real-bug F1 | %.2f | %.2f | %.2f |\n| Transient accuracy | %.2f | %.2f | %.2f |\n",
		len(cards), f1Mean, f1Min, f1Max, accMean, accMin, accMax)
}

func stats(xs []float64) (min, max, mean float64) {
	if len(xs) == 0 {
		return 0, 0, 0
	}
	min, max = xs[0], xs[0]
	var sum float64
	for _, x := range xs {
		if x < min {
			min = x
		}
		if x > max {
			max = x
		}
		sum += x
	}
	return min, max, sum / float64(len(xs))
}

func readScorecard(path string) (eval.Scorecard, error) {
	var sc eval.Scorecard
	data, err := os.ReadFile(path)
	if err != nil {
		return sc, err
	}
	return sc, json.Unmarshal(data, &sc)
}

func writeJSON(path string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Fatalf("eval: marshal %s: %v", path, err)
	}
	writeFile(path, string(data))
}

func writeFile(path, content string) {
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		log.Fatalf("eval: writing %s: %v", path, err)
	}
}
