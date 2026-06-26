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

	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v3"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

// Config is the in-memory representation of a project.yaml file.
type Config struct {
	ID                   string         `yaml:"id"         json:"id"`
	Name                 string         `yaml:"name"       json:"name"`
	ShortName            string         `yaml:"short_name" json:"short_name,omitempty"`
	Source               Source         `yaml:"source"     json:"source"`
	TestGrid             TestGrid       `yaml:"testgrid,omitempty"   json:"testgrid,omitempty"`
	Storage              Storage        `yaml:"storage"    json:"storage"`
	Discovery            Discovery      `yaml:"discovery,omitempty"  json:"discovery,omitempty"`
	Branding             Branding       `yaml:"branding"   json:"branding"`
	Categories           []CategoryRule `yaml:"categories,omitempty"            json:"categories,omitempty"`
	CategoryDisplayOrder []string       `yaml:"category_display_order,omitempty" json:"category_display_order,omitempty"`
	Artifacts            *Artifacts     `yaml:"artifacts,omitempty"  json:"artifacts,omitempty"`
	AI                   *AI            `yaml:"ai,omitempty"         json:"ai,omitempty"`
	Issues               *Issues        `yaml:"issues,omitempty"     json:"issues,omitempty"`

	// MinEngineVersion, when set, is the lowest engine release this config
	// expects (e.g. "1.4.0" or "v1.4.0"). The fetcher warns at startup if the
	// running engine is older, catching a consumer that adopted a newer config
	// field without bumping its pinned engine ref. Advisory only: a mismatch
	// warns, it does not fail the run. Not serialized to manifest.json.
	MinEngineVersion string `yaml:"min_engine_version,omitempty" json:"-"`

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

// Categorize returns the category id for a job name using the config's rules.
// See CategorizeJob for the matching semantics.
func (c *Config) Categorize(name string) string {
	return CategorizeJob(name, c.Categories)
}

// CategorizeJob returns the category id for a job name by evaluating rules in
// order (first case-insensitive substring match wins). It returns "" when no
// rules are configured (ungrouped dashboard) and "other" when rules exist but
// none match. Used by every job-discovery path so grouping is consistent
// regardless of source.
func CategorizeJob(name string, rules []CategoryRule) string {
	if len(rules) == 0 {
		return ""
	}
	lower := strings.ToLower(name)
	for _, r := range rules {
		if r.Match == "" || r.ID == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(r.Match)) {
			return r.ID
		}
	}
	return "other"
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
// Only used when discovery.source is "testgrid".
type TestGrid struct {
	Dashboard string `yaml:"dashboard" json:"dashboard"`
}

// Storage configures the artifact store that holds the project's Prow builds.
// The engine does not assume Google Cloud Storage: Provider is required and
// selects the backend, and the optional *Base fields point the engine at a
// project's own endpoints.
//
//	provider: gcs    -> native Google Cloud Storage (kubernetes.io Prow).
//	provider: gcsweb -> any gcsweb HTTP gateway fronting a bucket (e.g. an S3
//	                    bucket behind gcsweb.istio.io); set base to that gateway.
type Storage struct {
	Provider string `yaml:"provider" json:"provider"`
	Bucket   string `yaml:"bucket"   json:"bucket"`
	// Base is the gcsweb gateway root serving raw objects and HTML listings,
	// e.g. "https://gcsweb.istio.io/s3". Required for the gcsweb provider.
	Base string `yaml:"base,omitempty" json:"base,omitempty"`
	// WebBase overrides the human-browsable link root (defaults to the
	// kubernetes gcsweb for gcs, or Base for gcsweb).
	WebBase string `yaml:"web_base,omitempty" json:"web_base,omitempty"`
	// ProwBase overrides the Prow deck deep-link root, e.g.
	// "https://prow.istio.io/view/s3".
	ProwBase string `yaml:"prow_base,omitempty" json:"prow_base,omitempty"`
}

// Discovery selects how the fetcher finds the project's jobs.
//
//	source: testgrid (default) -> kubernetes/test-infra job YAMLs filtered by
//	                              testgrid.dashboard. The k8s ecosystem path.
//	source: bucket             -> list the storage bucket's own job indexes
//	                              (logs/ and pr-logs/directory/). Works for any
//	                              Prow instance regardless of config repo.
type Discovery struct {
	Source string `yaml:"source,omitempty" json:"source,omitempty"`
	// JobFilters, when set, keeps only discovered job names that contain one
	// of these substrings. Only used by the bucket source; omit to take every
	// job in the bucket (suitable for a project-dedicated bucket).
	JobFilters []string `yaml:"job_filters,omitempty" json:"job_filters,omitempty"`
}

// Discovery source names.
const (
	DiscoveryTestGrid = "testgrid"
	DiscoveryBucket   = "bucket"
)

// EffectiveDiscoverySource returns the configured discovery source, defaulting
// to "testgrid" when unset.
func (c *Config) EffectiveDiscoverySource() string {
	if c.Discovery.Source == "" {
		return DiscoveryTestGrid
	}
	return c.Discovery.Source
}

// StorageConfig maps the project's storage block onto a storage.Config.
// Validate() guarantees Provider is set, so no defaulting happens here.
func (c *Config) StorageConfig() storage.Config {
	return storage.Config{
		Provider: storage.Provider(c.Storage.Provider),
		Bucket:   c.Storage.Bucket,
		Base:     c.Storage.Base,
		WebBase:  c.Storage.WebBase,
		ProwBase: c.Storage.ProwBase,
	}
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

// Issue trigger names.
const (
	IssueTriggerPatterns   = "patterns"   // systemic recurring patterns
	IssueTriggerPersistent = "persistent" // failures with >=3 consecutive runs
)

// Issues configures optional auto-filing of GitHub issues for recurring
// patterns and persistent failures. Off by default; the fetcher only acts when
// `enabled: true` AND an ISSUE_TOKEN secret is present, so a misconfigured repo
// or a missing token is a no-op rather than a deploy failure.
type Issues struct {
	// Enabled turns the feature on for this consumer. Defaults to false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Repo is the target repo for filed issues. Defaults to
	// branding.source_repo. Point it at a repo you control (the token needs
	// issues:write there); auto-filing on an upstream community repo is rarely
	// wanted.
	Repo *SourceRepo `yaml:"repo,omitempty" json:"repo,omitempty"`
	// Triggers selects which signals open an issue: "patterns" and/or
	// "persistent". Defaults to both when empty.
	Triggers []string `yaml:"triggers,omitempty" json:"triggers,omitempty"`
	// Labels are applied to every filed issue. Defaults to ["prow-dashboard"].
	Labels []string `yaml:"labels,omitempty" json:"labels,omitempty"`
	// CommentOnRecovery posts a "recovered" comment when a tracked failure
	// clears. Defaults to true.
	CommentOnRecovery *bool `yaml:"comment_on_recovery,omitempty" json:"comment_on_recovery,omitempty"`
	// CloseOnRecovery also closes the issue on recovery. Defaults to false
	// (comment only, leave the issue open).
	CloseOnRecovery bool `yaml:"close_on_recovery,omitempty" json:"close_on_recovery,omitempty"`
	// MaxNewPerRun caps how many issues are created in a single fetch, a flood
	// guard for when many patterns appear at once or local state is lost.
	// Defaults to 5.
	MaxNewPerRun int `yaml:"max_new_per_run,omitempty" json:"max_new_per_run,omitempty"`
}

// EffectiveIssues resolves the issues config with defaults applied. Safe on a
// nil receiver (returns a disabled config).
func (c *Config) EffectiveIssues() Issues {
	out := Issues{}
	if c != nil && c.Issues != nil {
		out = *c.Issues
	}
	// Default the target repo to branding.source_repo only when `repo` is
	// omitted entirely. A partial `repo` (one of owner/name) is rejected by
	// Validate rather than silently completed from source_repo, which could
	// file issues on the wrong repo.
	if out.Repo == nil {
		if c != nil {
			out.Repo = &SourceRepo{Owner: c.Branding.SourceRepo.Owner, Name: c.Branding.SourceRepo.Name}
		}
	}
	if len(out.Triggers) == 0 {
		out.Triggers = []string{IssueTriggerPatterns, IssueTriggerPersistent}
	}
	if len(out.Labels) == 0 {
		out.Labels = []string{"prow-dashboard"}
	}
	if out.CommentOnRecovery == nil {
		t := true
		out.CommentOnRecovery = &t
	}
	if out.MaxNewPerRun <= 0 {
		out.MaxNewPerRun = 5
	}
	return out
}

// HasTrigger reports whether the given trigger is enabled.
func (i Issues) HasTrigger(name string) bool {
	for _, t := range i.Triggers {
		if t == name {
			return true
		}
	}
	return false
}

// AI configures the agentic failure-analysis pipeline: the endpoint and model
// to call, optional request headers, analysis concurrency, and the inlined
// agentic loop tuning.
type AI struct {
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

	// Concurrency caps how many failures are analyzed in parallel. Each
	// analysis is an independent sequence of model round-trips, so raising
	// this lets a batching endpoint (e.g. a self-hosted vLLM/TRT-LLM server)
	// work several investigations at once and cut wall-clock roughly in
	// proportion until the endpoint saturates. Defaults to 1 (sequential),
	// because the engine has no request-level backoff and a shared,
	// rate-limited provider (e.g. Copilot) can 429 under parallelism. Raise
	// it only for endpoints you control. Excluded from manifest.json.
	Concurrency int `yaml:"concurrency,omitempty" json:"-"`

	// Agentic holds the tool-calling loop tuning, inlined under `ai:` in
	// YAML (e.g. ai.max_iters, ai.timeout, ai.critique). All fields are
	// optional; zero values fall back to DefaultAgentic. The agentic loop
	// is the only analysis path: the model browses the build's artifacts
	// via the registered tools (filesystem + k8s by default) and returns a
	// single JSON verdict. A function-calling endpoint is required.
	Agentic Agentic `yaml:",inline" json:"agentic,omitempty"`
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
// values fall back to engine defaults defined in DefaultAgentic. Inlined under
// `ai:` in project.yaml.
type Agentic struct {
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

	// NOTE: recipe-driven skills are not gated by a config flag. Recipes
	// under <project_dir>/skills/*.yaml are consulted by the critique gate
	// whenever they are present and critique is enabled. Shipping recipe
	// files is itself the opt-in (the fetcher auto-enables critique when
	// recipes are present).

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

// DefaultAgentic is the zero-config fallback applied to the agentic loop tuning
// when a consumer omits a field. Tuned to match the validated spike: 15
// iterations is enough for deep exploration without runaway loops, and 5
// minutes is the wall-clock timeout. (The byte budgets are not configured
// here: the model/context budgets auto-size from the endpoint window and the
// GCS fetch ceiling is a fixed fetcher constant.)
var DefaultAgentic = Agentic{
	MaxIters:     15,
	Timeout:      5 * time.Minute,
	MinToolCalls: 0,
	MinGCSBytes:  0,
	Critique: AgenticCritique{
		Enabled:    false,
		MaxRetries: 2,
	},
}

// EffectiveAgentic returns the resolved agentic tuning with defaults applied
// for any zero-valued field. Safe to call on a nil receiver (returns
// DefaultAgentic).
func (a *AI) EffectiveAgentic() Agentic {
	out := DefaultAgentic
	if a == nil {
		return out
	}
	if a.Agentic.MaxIters > 0 {
		out.MaxIters = a.Agentic.MaxIters
	}
	if a.Agentic.Timeout > 0 {
		out.Timeout = a.Agentic.Timeout
	}
	if a.Agentic.MinToolCalls > 0 {
		out.MinToolCalls = a.Agentic.MinToolCalls
	}
	if a.Agentic.MinGCSBytes > 0 {
		out.MinGCSBytes = a.Agentic.MinGCSBytes
	}
	out.Critique.Enabled = a.Agentic.Critique.Enabled
	if a.Agentic.Critique.MaxRetries > 0 {
		out.Critique.MaxRetries = a.Agentic.Critique.MaxRetries
	}
	out.SingleToolCall = a.Agentic.SingleToolCall
	out.EvidenceInjection = a.Agentic.EvidenceInjection
	if len(a.Agentic.Tools) > 0 {
		out.Tools = append([]string(nil), a.Agentic.Tools...)
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
	require("storage.provider", c.Storage.Provider)
	require("storage.bucket", c.Storage.Bucket)
	require("branding.title", c.Branding.Title)
	require("branding.base_path", c.Branding.BasePath)
	require("branding.site_url", c.Branding.SiteURL)
	require("branding.source_repo.owner", c.Branding.SourceRepo.Owner)
	require("branding.source_repo.name", c.Branding.SourceRepo.Name)

	switch c.EffectiveDiscoverySource() {
	case DiscoveryTestGrid:
		require("testgrid.dashboard", c.TestGrid.Dashboard)
	case DiscoveryBucket:
		// No testgrid dashboard needed; jobs come from the bucket itself.
	default:
		missing = append(missing, fmt.Sprintf("discovery.source %q (want %q or %q)",
			c.Discovery.Source, DiscoveryTestGrid, DiscoveryBucket))
	}

	switch c.Storage.Provider {
	case "", string(storage.ProviderGCS):
		// Empty is already reported by require above; gcs needs no extra fields.
	case string(storage.ProviderGCSWeb):
		require("storage.base (required for the gcsweb provider)", c.Storage.Base)
	default:
		missing = append(missing, fmt.Sprintf("storage.provider %q (want %q or %q)",
			c.Storage.Provider, storage.ProviderGCS, storage.ProviderGCSWeb))
	}

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

	// Evidence injection hooks the critique retry path, so it is inert
	// without critique. Reject the misconfiguration at load rather than
	// silently doing nothing. (Note: critique is also auto-enabled by the
	// fetcher when skill recipes are present, so this only catches the
	// recipe-free case.)
	if c.AI != nil && c.AI.Agentic.EvidenceInjection && !c.AI.Agentic.Critique.Enabled {
		return fmt.Errorf("ai.evidence_injection requires ai.critique.enabled: true")
	}

	// Validate issue triggers when the feature is configured, so a typo fails
	// at load rather than silently never firing.
	if c.Issues != nil {
		for i, t := range c.Issues.Triggers {
			switch t {
			case IssueTriggerPatterns, IssueTriggerPersistent:
			default:
				return fmt.Errorf("issues.triggers[%d] %q is not valid (want %q or %q)",
					i, t, IssueTriggerPatterns, IssueTriggerPersistent)
			}
		}
		// A partial repo would otherwise be silently completed from
		// branding.source_repo, risking issues on the wrong repo.
		if r := c.Issues.Repo; r != nil && (r.Owner == "" || r.Name == "") {
			return fmt.Errorf("issues.repo requires both owner and name (omit issues.repo entirely to default to branding.source_repo)")
		}
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

// EngineVersionWarning returns an advisory message when the running engine is
// older than the config's MinEngineVersion, else "". engineVersion is the
// engine's own version (e.g. "v1.2.0"); a dev/unparseable value is treated as
// "cannot compare" and never warns. Both versions are normalized to a leading
// "v", so min_engine_version may be written with or without it.
func (c *Config) EngineVersionWarning(engineVersion string) string {
	if c.MinEngineVersion == "" {
		return ""
	}
	want := ensureVPrefix(c.MinEngineVersion)
	if !semver.IsValid(want) {
		return fmt.Sprintf("project.yaml min_engine_version %q is not a valid version; ignoring", c.MinEngineVersion)
	}
	got := ensureVPrefix(engineVersion)
	if !semver.IsValid(got) {
		// Local or untagged builds report dev[-<sha>]; cannot compare.
		if engineVersion == "" || engineVersion == "dev" || strings.HasPrefix(engineVersion, "dev-") {
			return ""
		}
		// Any other unparseable version signals a broken build embed; surface it.
		return fmt.Sprintf("engine version %q is not a recognized release; cannot verify min_engine_version %s", engineVersion, want)
	}
	if semver.Compare(got, want) < 0 {
		return fmt.Sprintf("engine version %s is older than this project's min_engine_version %s; "+
			"some project.yaml fields may be unsupported. Pin a newer engine release.", got, want)
	}
	return ""
}

func ensureVPrefix(s string) string {
	if s == "" || s[0] == 'v' {
		return s
	}
	return "v" + s
}
