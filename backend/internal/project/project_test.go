package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `
id: capz
name: "Cluster API Provider Azure"
short_name: "CAPZ"
testgrid:
  dashboard: "sig-cluster-lifecycle-cluster-api-provider-azure"
gcs:
  bucket: "kubernetes-ci-logs"
branding:
  title: "CAPZ Prow Dashboard"
  base_path: "/capz-prow-dashboard"
  site_url: "https://willie-yao.github.io/capz-prow-dashboard"
  source_repo:
    owner: "kubernetes-sigs"
    name: "cluster-api-provider-azure"
`

func TestParseValid(t *testing.T) {
	c, err := parse(strings.NewReader(validYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.ID != "capz" {
		t.Errorf("ID = %q, want %q", c.ID, "capz")
	}
	if c.TestGrid.Dashboard != "sig-cluster-lifecycle-cluster-api-provider-azure" {
		t.Errorf("TestGrid.Dashboard = %q", c.TestGrid.Dashboard)
	}
	if c.GCS.Bucket != "kubernetes-ci-logs" {
		t.Errorf("GCS.Bucket = %q", c.GCS.Bucket)
	}
	if c.Branding.Title != "CAPZ Prow Dashboard" {
		t.Errorf("Branding.Title = %q", c.Branding.Title)
	}
	if c.Branding.SourceRepo.Name != "cluster-api-provider-azure" {
		t.Errorf("Branding.SourceRepo.Name = %q", c.Branding.SourceRepo.Name)
	}
}

func TestParseMissingRequiredFields(t *testing.T) {
	const incomplete = `
id: capz
`
	_, err := parse(strings.NewReader(incomplete))
	if err == nil {
		t.Fatalf("expected error for incomplete config, got nil")
	}
	msg := err.Error()
	wantSubstrings := []string{
		"name",
		"testgrid.dashboard",
		"gcs.bucket",
		"branding.title",
		"branding.base_path",
		"branding.site_url",
		"branding.source_repo.owner",
		"branding.source_repo.name",
	}
	for _, w := range wantSubstrings {
		if !strings.Contains(msg, w) {
			t.Errorf("error missing field %q; got: %s", w, msg)
		}
	}
}

func TestParseUnknownField(t *testing.T) {
	const withTypo = `
id: capz
name: x
unknown_field: oops
testgrid:
  dashboard: x
gcs:
  bucket: x
branding:
  title: x
  base_path: /x
  site_url: https://example.com
  source_repo:
    owner: x
    name: x
`
	_, err := parse(strings.NewReader(withTypo))
	if err == nil {
		t.Fatalf("expected error for unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "unknown_field") {
		t.Errorf("error should name the unknown field; got: %v", err)
	}
}

func TestParseRejectsLegacySourcePaths(t *testing.T) {
	// test_infra_paths / file_prefix were removed when discovery moved
	// to dashboard-driven code search. Strict YAML parsing must reject
	// them so stale consumer configs surface a clear error at startup.
	const legacy = `
id: x
name: x
source:
  test_infra_paths: ["config/jobs/x"]
testgrid:
  dashboard: x
gcs:
  bucket: x
branding:
  title: x
  base_path: /x
  site_url: https://example.com
  source_repo:
    owner: x
    name: x
`
	_, err := parse(strings.NewReader(legacy))
	if err == nil {
		t.Fatal("expected error for legacy source.test_infra_paths, got nil")
	}
	if !strings.Contains(err.Error(), "test_infra_paths") {
		t.Errorf("error should mention the removed field; got: %v", err)
	}
}

func TestParseInvalidYAML(t *testing.T) {
	_, err := parse(strings.NewReader("not: : valid"))
	if err == nil {
		t.Fatalf("expected error for invalid YAML, got nil")
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "project.yaml")
	if err := os.WriteFile(path, []byte(validYAML), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ID != "capz" {
		t.Errorf("ID = %q, want capz", c.ID)
	}
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/project.yaml")
	if err == nil {
		t.Fatalf("expected error for missing file, got nil")
	}
}

func TestDisplayShortName(t *testing.T) {
	c := &Config{ID: "x"}
	if got := c.DisplayShortName(); got != "x" {
		t.Errorf("DisplayShortName fallback = %q, want %q", got, "x")
	}
	c.ShortName = "X-Project"
	if got := c.DisplayShortName(); got != "X-Project" {
		t.Errorf("DisplayShortName = %q, want %q", got, "X-Project")
	}
}

// validConfig returns a minimally-valid Config that Validate accepts. Tests
// below mutate it to exercise individual category-rule failure paths.
func validConfig() *Config {
	return &Config{
		ID:       "test",
		Name:     "Test",
		TestGrid: TestGrid{Dashboard: "test-dashboard"},
		GCS:      GCS{Bucket: "test-bucket"},
		Branding: Branding{
			Title:    "Test",
			BasePath: "/test",
			SiteURL:  "https://example.com",
			SourceRepo: SourceRepo{
				Owner: "owner",
				Name:  "name",
			},
		},
	}
}

func TestValidate_BaselinePasses(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("baseline config should be valid: %v", err)
	}
}

func TestValidate_CategoryRules(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"missing match", func(c *Config) {
			c.Categories = []CategoryRule{{ID: "e2e", Label: "E2E"}}
		}, "categories[0].match is required"},
		{"missing id", func(c *Config) {
			c.Categories = []CategoryRule{{Match: "e2e", Label: "E2E"}}
		}, "categories[0].id is required"},
		{"reserved id lowercase", func(c *Config) {
			c.Categories = []CategoryRule{{Match: "x", ID: "other", Label: "Other"}}
		}, "is reserved"},
		{"reserved id mixed case", func(c *Config) {
			c.Categories = []CategoryRule{{Match: "x", ID: "Other", Label: "Other"}}
		}, "is reserved"},
		{"id with surrounding whitespace", func(c *Config) {
			c.Categories = []CategoryRule{{Match: "e2e", ID: " e2e ", Label: "E2E"}}
		}, "surrounding whitespace"},
		{"valid custom rules", func(c *Config) {
			c.Categories = []CategoryRule{
				{Match: "conformance", ID: "conformance", Label: "Conformance"},
				{Match: "e2e", ID: "custom-e2e", Label: "Custom E2E"},
			}
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			assertValidate(t, c, tc.wantSub)
		})
	}
}

func TestValidate_CategoryDisplayOrder(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"unknown id rejected", func(c *Config) {
			c.Categories = []CategoryRule{{Match: "e2e", ID: "e2e", Label: "E2E"}}
			c.CategoryDisplayOrder = []string{"e2e", "made-up"}
		}, `"made-up" is not a declared category id`},
		{"empty entry rejected", func(c *Config) {
			c.Categories = []CategoryRule{{Match: "e2e", ID: "e2e", Label: "E2E"}}
			c.CategoryDisplayOrder = []string{"e2e", ""}
		}, "is empty"},
		{"other is allowed", func(c *Config) {
			c.Categories = []CategoryRule{{Match: "e2e", ID: "e2e", Label: "E2E"}}
			c.CategoryDisplayOrder = []string{"e2e", "other"}
		}, ""},
		{"consumer ids are honored", func(c *Config) {
			c.Categories = []CategoryRule{
				{Match: "e2e-aks", ID: "aks-e2e", Label: "AKS E2E"},
				{Match: "e2e", ID: "capz-e2e", Label: "CAPZ E2E"},
			}
			c.CategoryDisplayOrder = []string{"capz-e2e", "aks-e2e"}
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			assertValidate(t, c, tc.wantSub)
		})
	}
}

func TestEffectiveCategories(t *testing.T) {
	c := validConfig()
	if got := c.EffectiveCategories(); len(got) != 0 {
		t.Errorf("expected no rules when consumer omits categories, got %d (%+v)", len(got), got)
	}
	c.Categories = []CategoryRule{{Match: "x", ID: "x", Label: "X"}}
	got := c.EffectiveCategories()
	if len(got) != 1 || got[0].ID != "x" {
		t.Errorf("expected consumer rules to be returned, got %+v", got)
	}
}

func assertValidate(t *testing.T, c *Config, wantSub string) {
	t.Helper()
	err := c.Validate()
	if wantSub == "" {
		if err != nil {
			t.Fatalf("expected nil error, got: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", wantSub)
	}
	if !strings.Contains(err.Error(), wantSub) {
		t.Fatalf("error %q does not contain %q", err.Error(), wantSub)
	}
}

func TestAgentic_Effective(t *testing.T) {
	t.Run("nil receiver returns defaults with Enabled=false", func(t *testing.T) {
		got := (*Agentic)(nil).EffectiveAgentic()
		if !agenticEqual(got, DefaultAgentic) {
			t.Errorf("got %+v, want defaults %+v", got, DefaultAgentic)
		}
	})
	t.Run("zero struct returns defaults", func(t *testing.T) {
		got := (&Agentic{}).EffectiveAgentic()
		if !agenticEqual(got, DefaultAgentic) {
			t.Errorf("got %+v, want defaults %+v", got, DefaultAgentic)
		}
	})
	t.Run("Enabled flips through", func(t *testing.T) {
		got := (&Agentic{Enabled: true}).EffectiveAgentic()
		if !got.Enabled {
			t.Error("Enabled = false, want true")
		}
		if got.MaxIters != DefaultAgentic.MaxIters {
			t.Errorf("MaxIters = %d, want default %d", got.MaxIters, DefaultAgentic.MaxIters)
		}
	})
	t.Run("explicit limits override defaults", func(t *testing.T) {
		got := (&Agentic{
			Enabled:         true,
			MaxIters:        7,
			ModelByteBudget: 50_000,
			WallClock:       30 * time.Second,
		}).EffectiveAgentic()
		if got.MaxIters != 7 {
			t.Errorf("MaxIters = %d, want 7", got.MaxIters)
		}
		if got.ModelByteBudget != 50_000 {
			t.Errorf("ModelByteBudget = %d, want 50000", got.ModelByteBudget)
		}
		if got.WallClock != 30*time.Second {
			t.Errorf("WallClock = %v, want 30s", got.WallClock)
		}
		if got.GCSByteBudget != DefaultAgentic.GCSByteBudget {
			t.Errorf("GCSByteBudget = %d, want default %d", got.GCSByteBudget, DefaultAgentic.GCSByteBudget)
		}
	})
	t.Run("SingleToolCall flips through", func(t *testing.T) {
		if (&Agentic{Enabled: true}).EffectiveAgentic().SingleToolCall {
			t.Error("SingleToolCall should default to false")
		}
		if !(&Agentic{Enabled: true, SingleToolCall: true}).EffectiveAgentic().SingleToolCall {
			t.Error("SingleToolCall=true should pass through")
		}
	})
	t.Run("Tools list passes through", func(t *testing.T) {
		in := &Agentic{Tools: []string{"filesystem"}}
		got := in.EffectiveAgentic()
		if !equalStrings(got.Tools, []string{"filesystem"}) {
			t.Errorf("Tools = %v, want [filesystem]", got.Tools)
		}
		// Mutate input slice; effective copy must NOT change.
		in.Tools[0] = "mutated"
		if got.Tools[0] != "filesystem" {
			t.Errorf("EffectiveAgentic returned aliased slice; got %v after mutation", got.Tools)
		}
	})
	t.Run("empty Tools falls back to default empty", func(t *testing.T) {
		got := (&Agentic{}).EffectiveAgentic()
		if len(got.Tools) != 0 {
			t.Errorf("Tools = %v, want empty", got.Tools)
		}
	})
	t.Run("MinToolCalls defaults to zero", func(t *testing.T) {
		got := (&Agentic{Enabled: true}).EffectiveAgentic()
		if got.MinToolCalls != 0 {
			t.Errorf("MinToolCalls = %d, want 0", got.MinToolCalls)
		}
	})
	t.Run("MinToolCalls passes through when set", func(t *testing.T) {
		got := (&Agentic{Enabled: true, MinToolCalls: 3}).EffectiveAgentic()
		if got.MinToolCalls != 3 {
			t.Errorf("MinToolCalls = %d, want 3", got.MinToolCalls)
		}
	})
	t.Run("MinGCSBytes defaults to zero", func(t *testing.T) {
		got := (&Agentic{Enabled: true}).EffectiveAgentic()
		if got.MinGCSBytes != 0 {
			t.Errorf("MinGCSBytes = %d, want 0", got.MinGCSBytes)
		}
	})
	t.Run("MinGCSBytes passes through when set", func(t *testing.T) {
		got := (&Agentic{Enabled: true, MinGCSBytes: 200_000}).EffectiveAgentic()
		if got.MinGCSBytes != 200_000 {
			t.Errorf("MinGCSBytes = %d, want 200000", got.MinGCSBytes)
		}
	})
	t.Run("Critique disabled by default with default max retries", func(t *testing.T) {
		got := (&Agentic{Enabled: true}).EffectiveAgentic()
		if got.Critique.Enabled {
			t.Error("Critique.Enabled = true, want false (default)")
		}
		if got.Critique.MaxRetries != 2 {
			t.Errorf("Critique.MaxRetries = %d, want 2 (default)", got.Critique.MaxRetries)
		}
	})
	t.Run("Critique.Enabled flips through", func(t *testing.T) {
		got := (&Agentic{Enabled: true, Critique: AgenticCritique{Enabled: true}}).EffectiveAgentic()
		if !got.Critique.Enabled {
			t.Error("Critique.Enabled = false, want true")
		}
		// MaxRetries omitted in input → falls back to default 2.
		if got.Critique.MaxRetries != 2 {
			t.Errorf("Critique.MaxRetries = %d, want default 2", got.Critique.MaxRetries)
		}
	})
	t.Run("Critique.MaxRetries passes through when set", func(t *testing.T) {
		got := (&Agentic{
			Enabled:  true,
			Critique: AgenticCritique{Enabled: true, MaxRetries: 5},
		}).EffectiveAgentic()
		if got.Critique.MaxRetries != 5 {
			t.Errorf("Critique.MaxRetries = %d, want 5", got.Critique.MaxRetries)
		}
	})
}

// agenticEqual compares two Agentic structs without using ==, which would
// fail to compile once Tools (a slice) was added.
func agenticEqual(a, b Agentic) bool {
	return a.Enabled == b.Enabled &&
		a.Always == b.Always &&
		a.MaxIters == b.MaxIters &&
		a.ModelByteBudget == b.ModelByteBudget &&
		a.GCSByteBudget == b.GCSByteBudget &&
		a.WallClock == b.WallClock &&
		a.MinToolCalls == b.MinToolCalls &&
		a.MinGCSBytes == b.MinGCSBytes &&
		a.Critique == b.Critique &&
		a.SingleToolCall == b.SingleToolCall &&
		equalStrings(a.Tools, b.Tools)
}

// ---------- use_universal_path semantics ----------

func TestValidate_UniversalModuleRequiresUniversalPath(t *testing.T) {
	c := validConfig()
	c.AI = &AI{Module: "universal"}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error when ai.module=universal without use_universal_path")
	}
	if !strings.Contains(err.Error(), "use_universal_path") {
		t.Errorf("error %q should mention use_universal_path", err.Error())
	}
}

func TestValidate_UniversalModuleWithFlagPasses(t *testing.T) {
	c := validConfig()
	c.AI = &AI{Module: "universal", UseUniversalPath: true}
	if err := c.Validate(); err != nil {
		t.Fatalf("validation should pass when use_universal_path is true: %v", err)
	}
}

func TestValidate_UniversalCaseInsensitive(t *testing.T) {
	c := validConfig()
	c.AI = &AI{Module: "Universal"} // capitalized; should still error
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for case-insensitive universal without flag")
	}
}

func TestParse_UseUniversalPathField(t *testing.T) {
	yml := validYAML + "\nai:\n  use_universal_path: true\n  agentic:\n    enabled: true\n    tools: [filesystem]\n"
	c, err := parse(strings.NewReader(yml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.AI == nil || !c.AI.UseUniversalPath {
		t.Errorf("UseUniversalPath = %v, want true", c.AI != nil && c.AI.UseUniversalPath)
	}
	if c.AI.Agentic == nil || !equalStrings(c.AI.Agentic.Tools, []string{"filesystem"}) {
		t.Errorf("Agentic.Tools = %v, want [filesystem]", c.AI.Agentic)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
