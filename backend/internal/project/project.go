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
	"regexp"
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
	CAPI                 *CAPI          `yaml:"capi,omitempty"       json:"capi,omitempty"`
}

// CategoryRule maps a substring in a job name to a category id and display
// label. Rules are evaluated in order; the first match wins. When no rule
// matches, the job is categorized as "other".
//
// Rule order controls backend categorization precedence, not necessarily
// frontend display order — declare `category_display_order` separately
// when the two need to diverge (e.g. a broad rule must come after a
// specific one for matching, but should appear first in the UI).
//
// The engine ships a small generic default (see DefaultCategories) covering
// conformance, capi-e2e, upgrade, coverage, scalability, e2e. Consumers
// override or extend by listing their own rules in project.yaml.
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

// DefaultCategories is the project-neutral category set used when a consumer
// project.yaml does not declare its own categories.
var DefaultCategories = []CategoryRule{
	{Match: "conformance", ID: "conformance", Label: "Conformance"},
	{Match: "capi-e2e", ID: "capi-e2e", Label: "CAPI E2E"},
	{Match: "upgrade", ID: "upgrade", Label: "Upgrade"},
	{Match: "coverage", ID: "coverage", Label: "Coverage"},
	{Match: "scalability", ID: "scalability", Label: "Scalability"},
	{Match: "e2e", ID: "e2e", Label: "E2E"},
}

// EffectiveCategories returns the consumer's rules when present, otherwise
// the engine defaults.
func (c *Config) EffectiveCategories() []CategoryRule {
	if len(c.Categories) > 0 {
		return c.Categories
	}
	return DefaultCategories
}

// Source describes where in kubernetes/test-infra the project's prow
// job YAMLs live and how to filter them. TestInfraPaths may list one
// or more directories under the repo root; all are scanned and the
// union of matching jobs (per testgrid.dashboard) is fetched. FilePrefix
// is optional; when empty, every *.yaml in each path (except presets)
// is parsed and the dashboard label is the sole filter.
type Source struct {
	TestInfraPaths    []string `yaml:"test_infra_paths"   json:"test_infra_paths"`
	FilePrefix        string   `yaml:"file_prefix,omitempty" json:"file_prefix,omitempty"`
	IncludePresubmits bool     `yaml:"include_presubmits" json:"include_presubmits,omitempty"`
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

// CAPI holds Cluster-API-specific knobs. Phase A keeps them at the top
// level; Phase B will move them under a generic collector config section.
type CAPI struct {
	ClusterNamePrefix string `yaml:"cluster_name_prefix" json:"cluster_name_prefix"`
}

// Artifacts selects the per-build artifact collector used by the fetcher.
// Implementations live under backend/internal/collectors/.
type Artifacts struct {
	// Collector names the registered collector (e.g. "capi", "generic").
	// When unset or empty, the generic no-op collector is used.
	Collector string `yaml:"collector" json:"collector,omitempty"`
}

// AI selects the AI module used to build prompts and gather evidence for
// failure analysis. Implementations live under backend/internal/ai/modules/.
type AI struct {
	// Module names the registered AI module (e.g. "capi", "generic").
	// When unset, the module is inferred from artifacts.collector and
	// falls back to "generic".
	Module string `yaml:"module" json:"module,omitempty"`

	// Endpoint is the chat-completions URL for the AI provider. Any
	// OpenAI-compatible endpoint works (GitHub Copilot, OpenAI, Azure
	// OpenAI, Nvidia Dynamo, vLLM, Ollama). When unset, the fetcher
	// reads the AI_ENDPOINT environment variable; if that is also unset,
	// it defaults to GitHub Copilot at
	// https://api.githubcopilot.com/chat/completions. Excluded from
	// manifest.json so it never reaches the deployed Pages site.
	Endpoint string `yaml:"endpoint,omitempty" json:"-"`

	// Model is the model identifier the provider expects (e.g.
	// "claude-opus-4.7-xhigh" for Copilot, "gpt-4o" for OpenAI,
	// "meta/llama-3.1-70b-instruct" for an NVIDIA NIM). When unset, the
	// fetcher reads the AI_MODEL environment variable; if that is also
	// unset, it defaults to the engine's built-in Copilot model. MUST be
	// set (via YAML or AI_MODEL) when pointing at any non-Copilot
	// provider. Excluded from manifest.json so internal-only model
	// labels never reach the deployed Pages site; use AI_MODEL for
	// those.
	Model string `yaml:"model,omitempty" json:"-"`

	// Headers are extra HTTP headers merged into every request to the AI
	// provider after the defaults. Use this to add provider-specific
	// routing headers (e.g. "NIM-Function-Id") or to override the default
	// "Authorization: Bearer <token>" scheme for providers like Azure
	// OpenAI that use a custom auth header.
	//
	// Values are passed through verbatim; do not put secrets here. The
	// AI_TOKEN environment variable is the supported channel for the
	// bearer token.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// Evidence overrides the per-failure artifact sources the AI module
	// fetches. Fields left nil fall back to engine defaults. A non-nil
	// empty slice (e.g. `machine_logs: []`) disables that source entirely.
	// Currently interpreted only by the "capi" module; other modules
	// ignore it and log a warning at startup to surface misconfigurations.
	Evidence *Evidence `yaml:"evidence,omitempty" json:"evidence,omitempty"`

	// Agentic enables tool-calling-based artifact browsing. When enabled,
	// the AI module skips its curator-driven evidence collection (the
	// Evidence block) for failures the module opts into via the
	// AgenticPreferrer interface and instead lets the model browse the
	// build's artifact tree itself. Requires an AI endpoint that
	// supports OpenAI-style function calling.
	Agentic *Agentic `yaml:"agentic,omitempty" json:"agentic,omitempty"`

	// UseUniversalPath, when true, bypasses the module-routed pipeline in
	// favor of a project-agnostic agentic flow: per-build collector
	// evidence is skipped, the per-failure prompt is reduced to the test
	// failure context, and the agent is expected to discover everything
	// it needs via the registered tools (filesystem + k8s by default).
	//
	// Implies agentic.enabled=true regardless of the agentic block. There
	// is no curator fallback in this mode: an endpoint that rejects
	// function-calling surfaces as an explicit "unavailable" summary
	// instead of degrading silently to a tools-free prompt.
	//
	// Module is still validated for typos (it must be unset or name a
	// registered module) but its prompt / evidence functions are NOT
	// invoked. Existing consumers without this flag keep the
	// module-routed pipeline unchanged.
	UseUniversalPath bool `yaml:"use_universal_path,omitempty" json:"-"`
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

	// ModelByteBudget caps the total bytes returned to the model from
	// tool calls (across all rounds) per failure. Defaults to
	// DefaultAgentic.ModelByteBudget.
	ModelByteBudget int `yaml:"model_byte_budget,omitempty" json:"model_byte_budget,omitempty"`

	// GCSByteBudget caps the total bytes fetched from GCS (across all
	// tool calls) per failure. Defaults to DefaultAgentic.GCSByteBudget.
	GCSByteBudget int `yaml:"gcs_byte_budget,omitempty" json:"gcs_byte_budget,omitempty"`

	// WallClock caps the total time spent in the agentic loop per
	// failure. Defaults to DefaultAgentic.WallClock.
	WallClock time.Duration `yaml:"wall_clock,omitempty" json:"wall_clock,omitempty"`

	// MinToolCalls is the minimum number of tool calls the model must
	// make before its final JSON answer is accepted. When the model
	// returns a tools-free response with fewer tool calls than this
	// floor, the loop appends a nudge user-message asking it to
	// investigate further. Defaults to 0 (no floor), which preserves
	// the historical behavior for strong tool-using models (e.g. Claude
	// Opus) that already investigate deeply. Set to ~3 for weaker
	// open-weights models (e.g. Qwen3) that tend to finalize prematurely
	// from the prompt alone without inspecting any artifacts.
	//
	// Below-floor finals are still published if the model refuses to
	// investigate further (so triage always has SOMETHING to show), but
	// they are NOT written to the AI cache, ensuring the next fetcher
	// run retries the analysis with fresh tool calls. Cached results
	// from previous runs that fall below the current floor are also
	// invalidated and re-analyzed.
	MinToolCalls int `yaml:"min_tool_calls,omitempty" json:"min_tool_calls,omitempty"`

	// MinGCSBytes is the minimum cumulative bytes the model must have
	// fetched from GCS via tool calls before its final JSON answer is
	// accepted. Complements MinToolCalls because a model can satisfy a
	// tool-call floor with cheap list calls or tiny byte ranges and
	// still finalize without any meaningful evidence (observed against
	// Qwen3-235B: 6 tool calls returning 13 KB total, then a
	// fabricated "no specific error found" root cause). Tracked from
	// the agent's GCS budget counter, so it follows the same accounting
	// the engine already uses for cost capping. Defaults to 0 (no
	// floor). 200_000 (200 KB) is a reasonable starting value for
	// weaker open-weights models; raise gradually if the model keeps
	// parking at the floor with shallow evidence.
	//
	// Same publish/cache semantics as MinToolCalls: below-floor finals
	// are returned to the caller so triage has something, but they
	// are NOT cached, and cached entries below the current floor are
	// invalidated on the next read.
	MinGCSBytes int `yaml:"min_gcs_bytes,omitempty" json:"min_gcs_bytes,omitempty"`

	// Critique configures the L.4 Step 2 critique gate: after the
	// agentic loop produces a parseable tools-free final, run a
	// deterministic regex check on suggested_fix. If it punts (i.e.
	// the model emits a diagnostic / information-gathering TODO list
	// instead of a concrete remediation), append targeted feedback
	// asking the model to either drill in further OR use the strict
	// no-remediation escape hatch, then re-prompt up to MaxRetries
	// times. Drafts that still punt after retries are published but
	// not cached (mirrors MinToolCalls / MinGCSBytes anti-thrash).
	//
	// Defaults to disabled. Recommended for weaker open-weights
	// models (e.g. Qwen3-235B at 80% punt rate post-Step-1) where
	// the prompt-only L.4 Step 1 fixes proved insufficient. Strong
	// tool-using models (e.g. Claude Opus at 40% punt rate on the
	// same cases) benefit too but the cost/benefit trade-off is
	// per-consumer.
	Critique AgenticCritique `yaml:"critique,omitempty" json:"critique,omitempty"`

	// Skills configures the L.4 Step 3 recipe-driven evidence
	// layer. Only meaningful when Critique.Enabled is also true.
	// Recipes themselves live under <project_dir>/skills/*.yaml
	// and are loaded by the engine's skills package; this field
	// controls whether the loaded set is consulted by the critique
	// gate. See the AgenticSkills doc-comment for the on/off
	// semantics.
	Skills AgenticSkills `yaml:"skills,omitempty" json:"skills,omitempty"`

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
	// Enabled turns the critique gate on for this consumer. When
	// false (the default), the agentic loop's tools-free final is
	// accepted as-is and cached normally; behavior matches
	// pre-L.4-Step-2.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`

	// MaxRetries caps the number of extra re-prompt rounds the loop
	// spends per analysis when critique fails. Each retry consumes
	// one extra agentic iteration on top of the configured MaxIters.
	// Defaults to 2 when Enabled is true and MaxRetries is left at
	// 0; an explicit non-zero value overrides the default. Setting
	// max_retries: 0 in YAML therefore yields the engine default
	// (2), not "no retries"; this is consistent with the
	// MinToolCalls / MinGCSBytes "0 = use default" convention used
	// throughout this struct.
	MaxRetries int `yaml:"max_retries,omitempty" json:"max_retries,omitempty"`
}

// AgenticSkills is the per-project skill-set config (L.4 Step 3).
// Consumer-owned diagnostic recipes live under <project_dir>/skills/
// and feed the critique gate's evidence checks.
type AgenticSkills struct {
	// Enabled turns the L.4 Step 3 skills layer on for this
	// consumer. When false (the default), recipes under
	// <project_dir>/skills/ are still loaded and validated at
	// startup but the critique gate ignores them; behavior matches
	// pre-Step-3. When true, matched recipes inject their
	// procedure + required-evidence checks into the critique gate
	// and may extend the retry budget to give the agent room to
	// satisfy the missing evidence.
	//
	// Skills are only meaningful when Critique.Enabled is also true
	// (they extend the critique gate). With critique off, this flag
	// is a no-op.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// DefaultAgentic is the zero-config fallback applied when a consumer enables
// Agentic without overriding any limits. Tuned to match the validated spike:
// 15 iterations is enough for deep exploration without runaway loops, 300KB
// of model bytes keeps prompts well under context limits, 1GB of GCS bytes
// covers even very large build logs, and 5 minutes is the wall-clock cap.
var DefaultAgentic = Agentic{
	Enabled:         false,
	Always:          false,
	MaxIters:        15,
	ModelByteBudget: 300_000,
	GCSByteBudget:   1_000_000_000,
	WallClock:       5 * time.Minute,
	MinToolCalls:    0,
	MinGCSBytes:     0,
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
	if a.ModelByteBudget > 0 {
		out.ModelByteBudget = a.ModelByteBudget
	}
	if a.GCSByteBudget > 0 {
		out.GCSByteBudget = a.GCSByteBudget
	}
	if a.WallClock > 0 {
		out.WallClock = a.WallClock
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
	if len(a.Tools) > 0 {
		out.Tools = append([]string(nil), a.Tools...)
	}
	return out
}

// Evidence configures which artifacts the AI module fetches for each failure.
// All three fields are optional and independently fall back to engine defaults
// (see DefaultMachineLogs, DefaultControllerLogs, DefaultBuildLogPatterns).
//
// Scope: every field is interpreted only by the "capi" AI module, because the
// paths they refer to (clusters/<name>/machines/<vm>/ and
// clusters/bootstrap/logs/<ns>/<deployment>/<pod>/) are specific to the
// Cluster API artifact layout. Other modules (currently only "generic") ignore
// the entire block and log a warning at startup so misconfigurations don't
// silently slip through. A non-CAPI project that wants its own evidence shape
// should add a new AI module, not reuse these fields with a different meaning.
type Evidence struct {
	// MachineLogs lists filenames looked up under each cluster's
	// artifacts/clusters/<name>/machines/<vm>/<file> path. The AI module
	// fetches the tail of the first machine that has a non-empty URL for
	// each filename.
	MachineLogs []string `yaml:"machine_logs,omitempty" json:"machine_logs,omitempty"`

	// ControllerLogs lists management-cluster controller pod logs to fetch
	// from artifacts/clusters/bootstrap/logs/<namespace>/<deployment>/<pod>/<container_log>.
	// In YAML each entry may be a bare namespace string (shorthand for
	// {namespace: <string>}) or a full object with pod_name_regex and
	// container_log overrides.
	ControllerLogs []ControllerLogSelector `yaml:"controller_logs,omitempty" json:"controller_logs,omitempty"`

	// BuildLogPatterns is a list of regular expressions grepped against the
	// build log; matching lines (plus 2 lines of context) are included in
	// the AI prompt. Use to surface provider-specific error strings (e.g.
	// "SkuNotAvailable" for Azure) that the project-agnostic defaults miss.
	BuildLogPatterns []string `yaml:"build_log_patterns,omitempty" json:"build_log_patterns,omitempty"`
}

// IsZero reports whether every field of Evidence is unset (nil slice). An
// explicit `[]` counts as set so consumers can disable a default without
// triggering "ignored config" warnings.
func (e *Evidence) IsZero() bool {
	if e == nil {
		return true
	}
	return e.MachineLogs == nil && e.ControllerLogs == nil && e.BuildLogPatterns == nil
}

// ControllerLogSelector picks a controller pod log from the management
// cluster's artifacts/clusters/bootstrap/logs/<namespace>/<deployment>/<pod>/<container_log>
// layout. In project.yaml a bare string is shorthand for {namespace: <string>}
// with default pod_name_regex (".*") and container_log ("manager.log").
type ControllerLogSelector struct {
	Namespace    string `yaml:"namespace" json:"namespace"`
	PodNameRegex string `yaml:"pod_name_regex,omitempty" json:"pod_name_regex,omitempty"`
	ContainerLog string `yaml:"container_log,omitempty"  json:"container_log,omitempty"`
}

// UnmarshalYAML accepts either a bare string (the namespace) or a full
// mapping, so consumers can write either:
//
//	controller_logs:
//	  - capi-system
//	  - namespace: capi-kubeadm-control-plane-system
//	    pod_name_regex: "^kcp-"
func (s *ControllerLogSelector) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		s.Namespace = strings.TrimSpace(node.Value)
		return nil
	}
	type alias ControllerLogSelector
	var a alias
	if err := node.Decode(&a); err != nil {
		return err
	}
	*s = ControllerLogSelector(a)
	return nil
}

// DefaultMachineLogs are the machine log filenames fetched per failure when
// the consumer does not override. These are the universal Linux node logs;
// provider-specific files (boot.log, cloud-init-output.log) must be added by
// the consumer.
var DefaultMachineLogs = []string{"kubelet.log", "containerd.log", "journal.log"}

// DefaultControllerLogs are the CAPI core upstream controller-manager logs
// fetched per failure when the consumer does not override. Provider-specific
// controllers (capz-system, capv-system, ...) must be added by the consumer.
var DefaultControllerLogs = []ControllerLogSelector{
	{Namespace: "capi-system"},
	{Namespace: "capi-kubeadm-bootstrap-system"},
	{Namespace: "capi-kubeadm-control-plane-system"},
}

// DefaultBuildLogPatterns is the project-agnostic regex set greppped against
// the build log when the consumer does not override. Provider-specific
// patterns (e.g. SkuNotAvailable, GalleryImage) belong in the consumer's
// project.yaml.
var DefaultBuildLogPatterns = []string{
	`(?i)FAIL|FAILED|\[FAIL\]`,
	`(?i)timed?\s*out|timeout`,
	`(?i)ImagePullBackOff|ErrImagePull`,
	`(?i)CrashLoopBackOff`,
	`(?i)NotFound|not found`,
}

// defaultPodNameRegex matches any pod name; used when a selector omits it.
const defaultPodNameRegex = ".*"

// defaultContainerLog is the controller-manager pod's primary log file as
// emitted by the CAPI E2E framework.
const defaultContainerLog = "manager.log"

// EffectiveEvidence is the resolved per-failure evidence config. Regex
// strings have been compiled and any defaults filled in, so AI modules and
// collectors can use it directly without re-validating.
type EffectiveEvidence struct {
	// MachineLogs lists filenames to look up in each machine's logs map.
	MachineLogs []string
	// ControllerLogs lists selectors with all fields populated (defaults
	// applied where the consumer left them empty).
	ControllerLogs []ControllerLogSelector
	// PodNameRegexes is parallel to ControllerLogs; PodNameRegexes[i] is the
	// compiled regex for ControllerLogs[i].PodNameRegex.
	PodNameRegexes []*regexp.Regexp
	// BuildLogPatterns is the compiled form of the build-log grep patterns.
	BuildLogPatterns []*regexp.Regexp
}

// EffectiveEvidence resolves the consumer's ai.evidence block against engine
// defaults and compiles every regex so callers don't have to re-validate.
//
// Nil semantics: a nil top-level Evidence or a nil field means "use engine
// default". A non-nil empty slice (e.g. `machine_logs: []` in YAML, which
// decodes as a 0-length non-nil slice) is preserved as an explicit "disable
// this source" request.
//
// Regex compile errors are surfaced with a path pointing at the offending
// field so users can fix the YAML in one pass.
func (c *Config) EffectiveEvidence() (EffectiveEvidence, error) {
	var src Evidence
	if c.AI != nil && c.AI.Evidence != nil {
		src = *c.AI.Evidence
	}

	eff := EffectiveEvidence{
		MachineLogs:    src.MachineLogs,
		ControllerLogs: src.ControllerLogs,
	}
	if eff.MachineLogs == nil {
		eff.MachineLogs = append([]string{}, DefaultMachineLogs...)
	}
	if eff.ControllerLogs == nil {
		eff.ControllerLogs = append([]ControllerLogSelector{}, DefaultControllerLogs...)
	}

	for i := range eff.ControllerLogs {
		if strings.TrimSpace(eff.ControllerLogs[i].Namespace) == "" {
			return EffectiveEvidence{}, fmt.Errorf("ai.evidence.controller_logs[%d].namespace is required", i)
		}
		if eff.ControllerLogs[i].PodNameRegex == "" {
			eff.ControllerLogs[i].PodNameRegex = defaultPodNameRegex
		}
		if eff.ControllerLogs[i].ContainerLog == "" {
			eff.ControllerLogs[i].ContainerLog = defaultContainerLog
		}
		r, err := regexp.Compile(eff.ControllerLogs[i].PodNameRegex)
		if err != nil {
			return EffectiveEvidence{}, fmt.Errorf("ai.evidence.controller_logs[%d].pod_name_regex %q: %w", i, eff.ControllerLogs[i].PodNameRegex, err)
		}
		eff.PodNameRegexes = append(eff.PodNameRegexes, r)
	}

	patterns := src.BuildLogPatterns
	if patterns == nil {
		patterns = DefaultBuildLogPatterns
	}
	for i, p := range patterns {
		r, err := regexp.Compile(p)
		if err != nil {
			return EffectiveEvidence{}, fmt.Errorf("ai.evidence.build_log_patterns[%d] %q: %w", i, p, err)
		}
		eff.BuildLogPatterns = append(eff.BuildLogPatterns, r)
	}

	return eff, nil
}

// CollectorName returns the configured collector name, defaulting to "generic".
func (c *Config) CollectorName() string {
	if c.Artifacts == nil || strings.TrimSpace(c.Artifacts.Collector) == "" {
		return "generic"
	}
	return c.Artifacts.Collector
}

// AIModuleName returns the configured AI module name. When unset, it falls
// back to the collector name so the AI prompt naturally matches the artifact
// shape (e.g. capi collector → capi module). Final fallback is "generic".
func (c *Config) AIModuleName() string {
	if c.AI != nil && strings.TrimSpace(c.AI.Module) != "" {
		return c.AI.Module
	}
	if c.Artifacts != nil && c.Artifacts.Collector != "" {
		return c.Artifacts.Collector
	}
	return "generic"
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
	if len(c.Source.TestInfraPaths) == 0 {
		missing = append(missing, "source.test_infra_paths")
	}
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

	// Normalize paths: trim whitespace + surrounding slashes; dedup; reject empties.
	seen := make(map[string]struct{}, len(c.Source.TestInfraPaths))
	cleaned := make([]string, 0, len(c.Source.TestInfraPaths))
	for i, p := range c.Source.TestInfraPaths {
		norm := strings.Trim(strings.TrimSpace(p), "/")
		if norm == "" {
			return fmt.Errorf("source.test_infra_paths[%d] is empty after trimming", i)
		}
		if _, ok := seen[norm]; ok {
			continue
		}
		seen[norm] = struct{}{}
		cleaned = append(cleaned, norm)
	}
	c.Source.TestInfraPaths = cleaned
	c.Source.FilePrefix = strings.TrimSpace(c.Source.FilePrefix)

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
		for _, r := range DefaultCategories {
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

	// Compile evidence regexes early so YAML typos fail loud at startup,
	// not on the first cache-miss AI run.
	if _, err := c.EffectiveEvidence(); err != nil {
		return err
	}

	// "universal" is reserved for the use_universal_path flow. Picking it
	// as a normal module name without the flag would be a footgun: the
	// fetcher's normal module registry never registers it, so the run
	// would fail later with a confusing "unknown ai.module" error.
	if c.AI != nil && strings.EqualFold(strings.TrimSpace(c.AI.Module), "universal") && !c.AI.UseUniversalPath {
		return fmt.Errorf(`ai.module: "universal" requires ai.use_universal_path: true`)
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
