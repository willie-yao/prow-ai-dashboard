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
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the in-memory representation of a project.yaml file.
type Config struct {
	ID        string     `yaml:"id"         json:"id"`
	Name      string     `yaml:"name"       json:"name"`
	ShortName string     `yaml:"short_name" json:"short_name,omitempty"`
	Source    Source     `yaml:"source"     json:"source"`
	TestGrid  TestGrid   `yaml:"testgrid"   json:"testgrid"`
	GCS       GCS        `yaml:"gcs"        json:"gcs"`
	Branding  Branding   `yaml:"branding"   json:"branding"`
	Artifacts *Artifacts `yaml:"artifacts,omitempty" json:"artifacts,omitempty"`
	AI        *AI        `yaml:"ai,omitempty"        json:"ai,omitempty"`
	CAPI      *CAPI      `yaml:"capi,omitempty"      json:"capi,omitempty"`
}

// Source describes where in kubernetes/test-infra the project's prow
// job YAMLs live and how to filter them.
type Source struct {
	TestInfraPath string `yaml:"test_infra_path" json:"test_infra_path"`
	FilePrefix    string `yaml:"file_prefix"     json:"file_prefix"`
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
	// OpenAI, Nvidia Dynamo, vLLM, Ollama). When unset, defaults to
	// GitHub Copilot at https://api.githubcopilot.com/chat/completions.
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`

	// Model is the model identifier the provider expects (e.g.
	// "claude-opus-4.6" for Copilot, "gpt-4o" for OpenAI,
	// "meta/llama-3.1-70b-instruct" for an NVIDIA NIM). When unset,
	// defaults to the engine's built-in model for the GitHub Copilot
	// endpoint and MUST be set when pointing at any other provider.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`

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
}

// SupportedCollectors lists collector names accepted by Validate. Wiring
// happens in cmd/fetcher/main.go; keep this list in sync with the switch
// statement there.
var SupportedCollectors = []string{"generic", "capi"}

// SupportedAIModules lists AI module names accepted by Validate. Wiring
// happens in cmd/fetcher/main.go::buildAIModule; keep in sync.
var SupportedAIModules = []string{"generic", "capi"}

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
	require("source.test_infra_path", c.Source.TestInfraPath)
	require("source.file_prefix", c.Source.FilePrefix)
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

	if c.Artifacts != nil && c.Artifacts.Collector != "" {
		valid := false
		for _, name := range SupportedCollectors {
			if c.Artifacts.Collector == name {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("artifacts.collector %q is not supported (valid: %s)",
				c.Artifacts.Collector, strings.Join(SupportedCollectors, ", "))
		}
	}

	if c.AI != nil && c.AI.Module != "" {
		valid := false
		for _, name := range SupportedAIModules {
			if c.AI.Module == name {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("ai.module %q is not supported (valid: %s)",
				c.AI.Module, strings.Join(SupportedAIModules, ", "))
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
