// Package project loads and validates the per-project YAML config that
// describes which Prow/TestGrid data the dashboard should aggregate and
// how to brand the resulting site.
//
// One Config is loaded at fetcher startup and threaded through the rest
// of the backend. The same struct is also serialized to data/manifest.json
// for the frontend to consume at runtime.
package project

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the in-memory representation of a project.yaml file.
type Config struct {
	ID                   string         `yaml:"id"         json:"id"`
	Name                 string         `yaml:"name"       json:"name"`
	ShortName            string         `yaml:"short_name" json:"short_name,omitempty"`
	Source               Source         `yaml:"source"     json:"source"`
	TestGrid             TestGrid       `yaml:"testgrid"   json:"testgrid"`
	GCS                  GCS            `yaml:"gcs"        json:"gcs"`
	Branding             Branding       `yaml:"branding"   json:"branding"`
	Categories           []CategoryRule `yaml:"categories,omitempty"            json:"categories,omitempty"`
	CategoryDisplayOrder []string       `yaml:"category_display_order,omitempty" json:"category_display_order,omitempty"`
	Artifacts            *Artifacts     `yaml:"artifacts,omitempty"  json:"artifacts,omitempty"`
	AI                   *AI            `yaml:"ai,omitempty"         json:"ai,omitempty"`

	// ShortNamePrefix is a display-only hint derived at fetch time: the
	// longest "periodic-<x>-" prefix shared by a majority of discovered
	// periodic jobs. The frontend strips it from job names for compact
	// rendering. Not user-configurable; populated by the fetcher and
	// serialized into manifest.json.
	ShortNamePrefix string `yaml:"-" json:"short_name_prefix,omitempty"`
}

// CategoryRule maps a substring in a job name to a category id and display
// label. Rules are evaluated in order; first match wins. When no rule
// matches, the job is categorized as "other".
//
// Rule order controls categorization, not display order; declare
// `category_display_order` separately when the two need to diverge.
// Consumers can override or extend DefaultCategories from project.yaml.
type CategoryRule struct {
	// Match is the substring to look for in the job name. Comparison is
	// case-insensitive on both sides.
	Match string `yaml:"match" json:"match"`
	// ID is the category identifier used in JobSummary.Category and as the
	// key in dashboard grouping.
	ID string `yaml:"id" json:"id"`
	// Label is the human-readable section header rendered by the frontend.
	Label string `yaml:"label" json:"label"`
}

// EffectiveCategories returns the consumer's category rules. Categories are
// opt-in: when c.Categories is empty, the dashboard renders ungrouped (a
// single flat grid) and categorize() leaves every job's Category empty.
// Consumers who want a per-section layout declare rules in project.yaml.
func (c *Config) EffectiveCategories() []CategoryRule {
	return c.Categories
}

// Source controls how the fetcher behaves when discovering Prow jobs from
// the kubernetes/test-infra repository. Discovery itself is dashboard-driven:
// the engine asks GitHub code search for every YAML under config/jobs/ that
// mentions cfg.TestGrid.Dashboard, then keeps the jobs whose
// testgrid-dashboards annotation contains it. No per-project paths or
// filename filters are needed.
type Source struct {
	IncludePresubmits bool `yaml:"include_presubmits" json:"include_presubmits,omitempty"`
}

// TestGrid identifies the testgrid dashboard that owns the project's jobs.
type TestGrid struct {
	Dashboard string `yaml:"dashboard" json:"dashboard"`
}

// GCS identifies the bucket that holds the project's build artifacts.
type GCS struct {
	Bucket string `yaml:"bucket" json:"bucket"`
}

// Branding controls UI-facing strings and URLs.
type Branding struct {
	Title      string     `yaml:"title"       json:"title"`
	BasePath   string     `yaml:"base_path"   json:"base_path"`
	SiteURL    string     `yaml:"site_url"    json:"site_url"`
	SourceRepo SourceRepo `yaml:"source_repo" json:"source_repo"`
}

// SourceRepo points at the GitHub repo whose code these tests exercise.
// Used to build "view in source" deep links from failure stack traces.
type SourceRepo struct {
	Owner string `yaml:"owner" json:"owner"`
	Name  string `yaml:"name"  json:"name"`
}

// Artifacts selects the per-build artifact collector used by the fetcher.
// Implementations live under backend/internal/collectors/.
type Artifacts struct {
	// Collector names the registered collector (e.g. "generic"). When
	// unset, the generic no-op collector is used.
	Collector string `yaml:"collector" json:"collector,omitempty"`
}

// AI selects the AI module used to build prompts and gather evidence for
// failure analysis. Implementations live under backend/internal/ai/modules/.
type AI struct {
	// Module names the registered AI module (e.g. "generic"). Defaults
	// to "generic" when unset.
	Module string `yaml:"module" json:"module,omitempty"`

	// Endpoint is the OpenAI-compatible chat-completions URL. When unset,
	// reads AI_ENDPOINT env, then defaults to GitHub Copilot. Excluded
	// from manifest.json.
	Endpoint string `yaml:"endpoint,omitempty" json:"-"`

	// Model is the model identifier the provider expects. When unset,
	// reads AI_MODEL env, then defaults to the engine's Copilot model.
	// MUST be set when pointing at any non-Copilot provider. Excluded
	// from manifest.json.
	Model string `yaml:"model,omitempty" json:"-"`

	// Headers are extra HTTP headers merged into every AI request after
	// the defaults. Use for provider-specific routing headers or to
	// override the default Authorization scheme. Do not put secrets here;
	// AI_TOKEN is the supported channel for the bearer token.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// Agentic enables tool-calling-based artifact browsing. When on, the
	// module skips its curator-driven evidence collection for failures
	// that opt in (via AgenticPreferrer) and lets the model browse the
	// artifact tree itself. Requires a function-calling endpoint.
	Agentic *Agentic `yaml:"agentic,omitempty" json:"agentic,omitempty"`

	// UseUniversalPath bypasses the module-routed pipeline in favor of a
	// project-agnostic agentic flow: collector evidence is skipped, the
	// per-failure prompt is reduced to the test failure context, and the
	// agent discovers everything via the registered tools.
	//
	// Implies agentic.enabled=true. There is no curator fallback in this
	// mode: an endpoint that rejects function-calling surfaces as
	// "unavailable" rather than degrading to a tools-free prompt.
	UseUniversalPath bool `yaml:"use_universal_path,omitempty" json:"-"`

	// Concurrency caps how many failures are analyzed in parallel. Each
	// analysis is an independent sequence of model round-trips, so raising
	// this lets a batching endpoint (e.g. a self-hosted vLLM/TRT-LLM server)
	// work several investigations at once and cut wall-clock roughly in
	// proportion until the endpoint saturates. Defaults to 1 (sequential),
	// because the engine has no request-level backoff and a shared,
	// rate-limited provider (e.g. Copilot) can 429 under parallelism. Raise
	// it only for endpoints you control. Excluded from manifest.json.
	Concurrency int `yaml:"concurrency,omitempty" json:"-"`
}

// AnalysisConcurrency returns the number of failures to analyze in parallel,
// clamped to a minimum of 1 so an unset or invalid value preserves the
// original sequential behavior.
func (c *Config) AnalysisConcurrency() int {
	if c.AI == nil || c.AI.Concurrency < 1 {
		return 1
	}
	return c.AI.Concurrency
}

// Agentic configures the tool-calling AI loop. All fields are optional; zero
// values fall back to engine defaults defined in DefaultAgentic.
type Agentic struct {
	// Enabled turns the agentic pipeline on. When false (the default),
	// every failure is analyzed by the existing curator-driven pipeline
	// regardless of any other field.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// Always forces agentic mode for every failure the module analyzes.
	// When false, the module decides per-failure via its AgenticPreferrer
	// implementation (modules that don't implement it never go agentic).
	Always bool `yaml:"always,omitempty" json:"always,omitempty"`

	// MaxIters caps the number of tool-call rounds per failure. Defaults
	// to DefaultAgentic.MaxIters.
	MaxIters int `yaml:"max_iters,omitempty" json:"max_iters,omitempty"`

	// NOTE: the model-output, context (compaction), and GCS byte budgets are
	// NOT configurable. The model-output and context budgets are derived from
	// the endpoint's reported context window; the GCS fetch ceiling is a fixed
	// engine safety cap. See the fetcher's agentic wiring.

	// Timeout caps the total wall-clock time spent in the agentic loop
	// per failure. When hit, the in-flight request is cancelled and the
	// analysis errors out. Defaults to DefaultAgentic.Timeout.
	Timeout time.Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`

	// MinToolCalls is the minimum number of tool calls the model must
	// make before its final JSON answer is accepted. When the model
	// returns a tools-free response below this floor, the loop nudges it
	// to investigate further. Below-floor finals are still published
	// but NOT cached, so the next run retries. Defaults to 0 (no floor).
	MinToolCalls int `yaml:"min_tool_calls,omitempty" json:"min_tool_calls,omitempty"`

	// MinGCSBytes is the minimum cumulative bytes the model must fetch
	// via tool calls before its final answer is accepted. Complements
	// MinToolCalls because a model can satisfy a calls floor with cheap
	// list calls or tiny reads. Same publish/no-cache semantics as
	// MinToolCalls. Defaults to 0.
	MinGCSBytes int `yaml:"min_gcs_bytes,omitempty" json:"min_gcs_bytes,omitempty"`

	// Critique configures the critique gate: after the agentic loop
	// produces a parseable tools-free final, run a deterministic regex
	// check on suggested_fix. If it punts, append targeted feedback
	// and re-prompt up to MaxRetries times. Drafts that still punt
	// after retries are published but not cached.
	//
	// Defaults to disabled. Recommended for weaker tool-using models
	// where the prompt rules alone don't reliably prevent punt-shaped
	// answers.
	Critique AgenticCritique `yaml:"critique,omitempty" json:"critique,omitempty"`

	// Skills configures the recipe-driven evidence layer. Only
	// meaningful when Critique.Enabled is also true. Recipes live under
	// <project_dir>/skills/*.yaml; this field controls whether the
	// loaded set is consulted by the critique gate.
	Skills AgenticSkills `yaml:"skills,omitempty" json:"skills,omitempty"`

	// SingleToolCall makes the loop send at most one tool call per assistant
	// turn: when the model returns several tool calls at once, only the first
	// is executed and echoed into history, and the rest are dropped (the
	// model can re-request them on a later turn). Required for endpoints whose
	// chat template rejects multiple tool calls in one assistant message (the
	// stock Llama 3.x Instruct template raises "This model only supports
	// single tool-calls at once!"). Defaults to false; leave it off for
	// providers that support parallel tool calls (Copilot, OpenAI, Claude) so
	// they keep their round-trip efficiency.
	SingleToolCall bool `yaml:"single_tool_call,omitempty" json:"single_tool_call,omitempty"`

	// EvidenceInjection turns cited-but-unread artifacts into injected
	// evidence on a critique retry: instead of only re-prompting the model
	// to go read an artifact it cited but never opened, the engine fetches
	// that artifact and embeds its (capped) content in the retry feedback,
	// so a weak model that ignores "go read X" still gets the bytes in
	// front of it. Only meaningful when Critique.Enabled is also true (it
	// hooks the critique retry path). Best suited to large-context models,
	// since it adds the fetched evidence to the conversation. Defaults to
	// disabled.
	EvidenceInjection bool `yaml:"evidence_injection,omitempty" json:"evidence_injection,omitempty"`

	// NOTE: artifact-tree seeding (prepending the build's exact artifact
	// path list to the prompt) is always on and not configurable. It is
	// deterministic, capped, and a no-op when the listing is empty.

	// Tools selects which registered tool groups (e.g. "filesystem",
	// "k8s") or individual tool names (e.g. "k8s.discover_clusters") are
	// exposed to the model. When empty, the fetcher applies its default
	// (["filesystem", "k8s"]). Non-K8s projects (e.g. node-level test
	// failures) should set ["filesystem"] to avoid registering tier-2
	// schemas that mostly return empty on their artifact trees.
	Tools []string `yaml:"tools,omitempty" json:"tools,omitempty"`
}

// AgenticCritique is the per-project critique-gate config. See
// Agentic.Critique for the operational semantics.
type AgenticCritique struct {
	// Enabled turns the critique gate on for this consumer. When false
	// (the default), the agentic loop's tools-free final is accepted
	// as-is and cached normally.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`

	// MaxRetries caps the number of extra re-prompt rounds the loop
	// spends per analysis when critique fails. Each retry consumes one
	// extra agentic iteration on top of MaxIters. Defaults to 2 when
	// Enabled is true and MaxRetries is 0 (consistent with the "0 =
	// use default" convention).
	MaxRetries int `yaml:"max_retries,omitempty" json:"max_retries,omitempty"`
}

// AgenticSkills is the per-project skill-set config. Consumer-owned
// diagnostic recipes live under <project_dir>/skills/ and feed the
// critique gate's evidence checks.
type AgenticSkills struct {
	// Enabled turns the skills layer on for this consumer. When false
	// (the default), recipes under <project_dir>/skills/ are still
	// loaded and validated at startup but the critique gate ignores
	// them. When true, matched recipes inject their procedure +
	// required-evidence checks into the critique gate and may extend
	// the retry budget. Only meaningful when Critique.Enabled is also
	// true; otherwise a no-op.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// DefaultAgentic is the zero-config fallback applied when a consumer enables
// Agentic without overriding any limits. Tuned to match the validated spike:
// 15 iterations is enough for deep exploration without runaway loops, and 5
// minutes is the wall-clock timeout. (The byte budgets are not configured
// here: the model/context budgets auto-size from the endpoint window and the
// GCS fetch ceiling is a fixed fetcher constant.)
var DefaultAgentic = Agentic{
	Enabled:      false,
	Always:       false,
	MaxIters:     15,
	Timeout:      5 * time.Minute,
	MinToolCalls: 0,
	MinGCSBytes:  0,
	Critique: AgenticCritique{
		Enabled:    false,
		MaxRetries: 2,
	},
}

// EffectiveAgentic returns the resolved Agentic config with defaults applied
// for any zero-valued limits. Safe to call on a nil receiver (returns
// DefaultAgentic with Enabled=false).
func (a *Agentic) EffectiveAgentic() Agentic {
	out := DefaultAgentic
	if a == nil {
		return out
	}
	out.Enabled = a.Enabled
	out.Always = a.Always
	if a.MaxIters > 0 {
		out.MaxIters = a.MaxIters
	}
	if a.Timeout > 0 {
		out.Timeout = a.Timeout
	}
	if a.MinToolCalls > 0 {
		out.MinToolCalls = a.MinToolCalls
	}
	if a.MinGCSBytes > 0 {
		out.MinGCSBytes = a.MinGCSBytes
	}
	out.Critique.Enabled = a.Critique.Enabled
	if a.Critique.MaxRetries > 0 {
		out.Critique.MaxRetries = a.Critique.MaxRetries
	}
	out.Skills.Enabled = a.Skills.Enabled
	out.SingleToolCall = a.SingleToolCall
	out.EvidenceInjection = a.EvidenceInjection
	if len(a.Tools) > 0 {
		out.Tools = append([]string(nil), a.Tools...)
	}
	return out
}

// CollectorName returns the configured collector name, defaulting to "generic".
func (c *Config) CollectorName() string {
	if c.Artifacts == nil || strings.TrimSpace(c.Artifacts.Collector) == "" {
		return "generic"
	}
	return c.Artifacts.Collector
}

// Load reads and validates a project.yaml file from disk.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	cfg, err := parse(f)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, nil
}

// LoadDir reads <dir>/project.yaml and <dir>/prompts/system.md. Both are
// mandatory: a missing or whitespace-only prompt is a hard error because AI
// analysis is the main value the dashboard provides. The returned prompt is
// the raw consumer addendum; the caller is expected to wrap it with
// ai.ComposeSystemPrompt before handing it to the AI service.
func LoadDir(dir string) (*Config, string, error) {
	cfg, err := Load(filepath.Join(dir, "project.yaml"))
	if err != nil {
		return nil, "", err
	}
	promptPath := filepath.Join(dir, "prompts", "system.md")
	data, err := os.ReadFile(promptPath)
	if err != nil {
		return nil, "", fmt.Errorf("AI analysis requires %s; see https://github.com/willie-yao/prow-ai-dashboard/blob/main/docs/writing-prompts.md (%w)", promptPath, err)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return nil, "", fmt.Errorf("AI analysis requires non-empty %s; see https://github.com/willie-yao/prow-ai-dashboard/blob/main/docs/writing-prompts.md", promptPath)
	}
	return cfg, prompt, nil
}

// parse decodes YAML in strict mode (unknown fields are errors) and
// runs validation.
func parse(r io.Reader) (*Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate reports every missing required field in one error message so
// users can fix the YAML in a single pass instead of iterating.
func (c *Config) Validate() error {
	var missing []string
	require := func(name, val string) {
		if strings.TrimSpace(val) == "" {
			missing = append(missing, name)
		}
	}
	require("id", c.ID)
	require("name", c.Name)
	require("testgrid.dashboard", c.TestGrid.Dashboard)
	require("gcs.bucket", c.GCS.Bucket)
	require("branding.title", c.Branding.Title)
	require("branding.base_path", c.Branding.BasePath)
	require("branding.site_url", c.Branding.SiteURL)
	require("branding.source_repo.owner", c.Branding.SourceRepo.Owner)
	require("branding.source_repo.name", c.Branding.SourceRepo.Name)

	if len(missing) > 0 {
		return fmt.Errorf("project config missing required field(s): %s", strings.Join(missing, ", "))
	}

	for i, r := range c.Categories {
		match := strings.TrimSpace(r.Match)
		id := strings.TrimSpace(r.ID)
		if match == "" {
			return fmt.Errorf("categories[%d].match is required", i)
		}
		if id == "" {
			return fmt.Errorf("categories[%d].id is required", i)
		}
		if id != r.ID {
			return fmt.Errorf("categories[%d].id %q must not have surrounding whitespace", i, r.ID)
		}
		if strings.EqualFold(id, "other") {
			return fmt.Errorf("categories[%d].id %q is reserved for the implicit fallback bucket", i, r.ID)
		}
	}

	if len(c.CategoryDisplayOrder) > 0 {
		known := map[string]struct{}{"other": {}}
		for _, r := range c.Categories {
			known[r.ID] = struct{}{}
		}
		for i, id := range c.CategoryDisplayOrder {
			if strings.TrimSpace(id) == "" {
				return fmt.Errorf("category_display_order[%d] is empty", i)
			}
			if _, ok := known[id]; !ok {
				return fmt.Errorf("category_display_order[%d] %q is not a declared category id", i, id)
			}
		}
	}

	// "universal" is reserved for the use_universal_path flow. Picking it
	// as a normal module name without the flag would be a footgun: the
	// fetcher's normal module registry never registers it, so the run
	// would fail later with a confusing "unknown ai.module" error.
	if c.AI != nil && strings.EqualFold(strings.TrimSpace(c.AI.Module), "universal") && !c.AI.UseUniversalPath {
		return fmt.Errorf(`ai.module: "universal" requires ai.use_universal_path: true`)
	}

	// Evidence injection hooks the critique retry path, so it is inert
	// without critique. Reject the misconfiguration at load rather than
	// silently doing nothing.
	if c.AI != nil && c.AI.Agentic != nil && c.AI.Agentic.EvidenceInjection && !c.AI.Agentic.Critique.Enabled {
		return fmt.Errorf("ai.agentic.evidence_injection requires ai.agentic.critique.enabled: true")
	}

	// Skills only feed the critique gate's missing-evidence check, so
	// skills.enabled is inert without critique. Same footgun as
	// evidence_injection above; reject it at load.
	if c.AI != nil && c.AI.Agentic != nil && c.AI.Agentic.Skills.Enabled && !c.AI.Agentic.Critique.Enabled {
		return fmt.Errorf("ai.agentic.skills.enabled requires ai.agentic.critique.enabled: true")
	}
	return nil
}

// DisplayShortName returns ShortName, falling back to ID when unset.
func (c *Config) DisplayShortName() string {
	if c.ShortName != "" {
		return c.ShortName
	}
	return c.ID
}
