package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validYAML = `
id: capz
name: "Cluster API Provider Azure"
short_name: "CAPZ"
source:
  test_infra_path: "config/jobs/kubernetes-sigs/cluster-api-provider-azure"
  file_prefix: "cluster-api-provider-azure-"
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
capi:
  cluster_name_prefix: "capz-e2e"
`

func TestParseValid(t *testing.T) {
	c, err := parse(strings.NewReader(validYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.ID != "capz" {
		t.Errorf("ID = %q, want %q", c.ID, "capz")
	}
	if c.Source.TestInfraPath != "config/jobs/kubernetes-sigs/cluster-api-provider-azure" {
		t.Errorf("Source.TestInfraPath = %q", c.Source.TestInfraPath)
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
	if c.CAPI == nil || c.CAPI.ClusterNamePrefix != "capz-e2e" {
		t.Errorf("CAPI.ClusterNamePrefix not set as expected: %+v", c.CAPI)
	}
}

func TestParseMissingRequiredFields(t *testing.T) {
	const incomplete = `
id: capz
source:
  test_infra_path: "x"
`
	_, err := parse(strings.NewReader(incomplete))
	if err == nil {
		t.Fatalf("expected error for incomplete config, got nil")
	}
	msg := err.Error()
	// Every absent required field should be named in the error so users
	// can fix the YAML in one pass.
	wantSubstrings := []string{
		"name",
		"source.file_prefix",
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
source:
  test_infra_path: x
  file_prefix: x
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
		ID:   "test",
		Name: "Test",
		Source: Source{
			TestInfraPath: "config/jobs/test",
			FilePrefix:    "test-",
		},
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
			c.CategoryDisplayOrder = []string{"e2e", "made-up"}
		}, `"made-up" is not a declared category id`},
		{"empty entry rejected", func(c *Config) {
			c.CategoryDisplayOrder = []string{"e2e", ""}
		}, "is empty"},
		{"other is allowed", func(c *Config) {
			c.CategoryDisplayOrder = []string{"e2e", "other"}
		}, ""},
		{"default category ids are honored", func(c *Config) {
			c.CategoryDisplayOrder = []string{"conformance", "e2e"}
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
	if got := c.EffectiveCategories(); len(got) != len(DefaultCategories) {
		t.Errorf("expected %d default rules, got %d", len(DefaultCategories), len(got))
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

// --- Evidence schema ---

func TestEffectiveEvidence_DefaultsWhenAIBlockAbsent(t *testing.T) {
	c := validConfig()
	c.AI = nil
	ev, err := c.EffectiveEvidence()
	if err != nil {
		t.Fatalf("EffectiveEvidence: %v", err)
	}
	if got, want := len(ev.MachineLogs), len(DefaultMachineLogs); got != want {
		t.Errorf("MachineLogs len = %d, want %d", got, want)
	}
	if got, want := len(ev.ControllerLogs), len(DefaultControllerLogs); got != want {
		t.Errorf("ControllerLogs len = %d, want %d", got, want)
	}
	if len(ev.PodNameRegexes) != len(ev.ControllerLogs) {
		t.Errorf("PodNameRegexes len = %d, want %d", len(ev.PodNameRegexes), len(ev.ControllerLogs))
	}
	if got, want := len(ev.BuildLogPatterns), len(DefaultBuildLogPatterns); got != want {
		t.Errorf("BuildLogPatterns len = %d, want %d", got, want)
	}
	for i, sel := range ev.ControllerLogs {
		if sel.PodNameRegex != defaultPodNameRegex {
			t.Errorf("ControllerLogs[%d].PodNameRegex = %q, want default %q", i, sel.PodNameRegex, defaultPodNameRegex)
		}
		if sel.ContainerLog != defaultContainerLog {
			t.Errorf("ControllerLogs[%d].ContainerLog = %q, want default %q", i, sel.ContainerLog, defaultContainerLog)
		}
	}
}

func TestEffectiveEvidence_EmptyEvidenceBlockUsesDefaults(t *testing.T) {
	// `evidence: {}` decodes to a non-nil pointer to a zero-valued struct.
	// Each nil slice within it should still fall back to defaults.
	c := validConfig()
	c.AI = &AI{Module: "capi", Evidence: &Evidence{}}
	ev, err := c.EffectiveEvidence()
	if err != nil {
		t.Fatalf("EffectiveEvidence: %v", err)
	}
	if len(ev.MachineLogs) != len(DefaultMachineLogs) {
		t.Errorf("MachineLogs len = %d, want %d", len(ev.MachineLogs), len(DefaultMachineLogs))
	}
	if len(ev.ControllerLogs) != len(DefaultControllerLogs) {
		t.Errorf("ControllerLogs len = %d, want %d", len(ev.ControllerLogs), len(DefaultControllerLogs))
	}
	if len(ev.BuildLogPatterns) != len(DefaultBuildLogPatterns) {
		t.Errorf("BuildLogPatterns len = %d, want %d", len(ev.BuildLogPatterns), len(DefaultBuildLogPatterns))
	}
}

func TestEffectiveEvidence_ExplicitEmptySlicesDisableSources(t *testing.T) {
	c := validConfig()
	c.AI = &AI{
		Module: "capi",
		Evidence: &Evidence{
			MachineLogs:      []string{},
			ControllerLogs:   []ControllerLogSelector{},
			BuildLogPatterns: []string{},
		},
	}
	ev, err := c.EffectiveEvidence()
	if err != nil {
		t.Fatalf("EffectiveEvidence: %v", err)
	}
	if len(ev.MachineLogs) != 0 {
		t.Errorf("MachineLogs should be empty, got %v", ev.MachineLogs)
	}
	if len(ev.ControllerLogs) != 0 {
		t.Errorf("ControllerLogs should be empty, got %v", ev.ControllerLogs)
	}
	if len(ev.BuildLogPatterns) != 0 {
		t.Errorf("BuildLogPatterns should be empty, got %v", ev.BuildLogPatterns)
	}
}

func TestEffectiveEvidence_OmittedFieldFallsBack(t *testing.T) {
	// Consumer sets only machine_logs; controller_logs and build_log_patterns
	// should still get engine defaults.
	c := validConfig()
	c.AI = &AI{
		Module: "capi",
		Evidence: &Evidence{
			MachineLogs: []string{"kubelet.log", "kern.log"},
		},
	}
	ev, err := c.EffectiveEvidence()
	if err != nil {
		t.Fatalf("EffectiveEvidence: %v", err)
	}
	if got, want := ev.MachineLogs, []string{"kubelet.log", "kern.log"}; !equalStrings(got, want) {
		t.Errorf("MachineLogs = %v, want %v", got, want)
	}
	if len(ev.ControllerLogs) != len(DefaultControllerLogs) {
		t.Errorf("ControllerLogs should default to %d entries, got %d", len(DefaultControllerLogs), len(ev.ControllerLogs))
	}
	if len(ev.BuildLogPatterns) != len(DefaultBuildLogPatterns) {
		t.Errorf("BuildLogPatterns should default, got %d entries", len(ev.BuildLogPatterns))
	}
}

func TestEffectiveEvidence_NilSliceIsTreatedAsOmitted(t *testing.T) {
	// `machine_logs: null` (YAML null) and `machine_logs:` (no value) both
	// decode to a nil slice. Defaults must apply.
	const yaml = `
id: capz
name: x
source: {test_infra_path: x, file_prefix: x-}
testgrid: {dashboard: x}
gcs: {bucket: x}
branding:
  title: x
  base_path: /x
  site_url: https://x
  source_repo: {owner: o, name: n}
ai:
  module: capi
  evidence:
    machine_logs:
    build_log_patterns: null
`
	c, err := parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ev, err := c.EffectiveEvidence()
	if err != nil {
		t.Fatalf("EffectiveEvidence: %v", err)
	}
	if len(ev.MachineLogs) != len(DefaultMachineLogs) {
		t.Errorf("nil machine_logs should fall back to default, got %d entries", len(ev.MachineLogs))
	}
	if len(ev.BuildLogPatterns) != len(DefaultBuildLogPatterns) {
		t.Errorf("nil build_log_patterns should fall back to default, got %d entries", len(ev.BuildLogPatterns))
	}
}

func TestEffectiveEvidence_BareStringControllerLogIsPromoted(t *testing.T) {
	const yaml = `
id: capz
name: x
source: {test_infra_path: x, file_prefix: x-}
testgrid: {dashboard: x}
gcs: {bucket: x}
branding:
  title: x
  base_path: /x
  site_url: https://x
  source_repo: {owner: o, name: n}
ai:
  module: capi
  evidence:
    controller_logs:
      - capi-system
      - namespace: capi-kubeadm-control-plane-system
        pod_name_regex: "^kcp-"
        container_log: manager.log
`
	c, err := parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ev, err := c.EffectiveEvidence()
	if err != nil {
		t.Fatalf("EffectiveEvidence: %v", err)
	}
	if len(ev.ControllerLogs) != 2 {
		t.Fatalf("ControllerLogs len = %d, want 2", len(ev.ControllerLogs))
	}
	if got := ev.ControllerLogs[0]; got.Namespace != "capi-system" || got.PodNameRegex != defaultPodNameRegex || got.ContainerLog != defaultContainerLog {
		t.Errorf("bare-string entry not promoted with defaults: %+v", got)
	}
	if got := ev.ControllerLogs[1]; got.Namespace != "capi-kubeadm-control-plane-system" || got.PodNameRegex != "^kcp-" {
		t.Errorf("object entry not parsed: %+v", got)
	}
}

func TestEffectiveEvidence_InvalidPodNameRegexFailsAtLoad(t *testing.T) {
	c := validConfig()
	c.AI = &AI{
		Module: "capi",
		Evidence: &Evidence{
			ControllerLogs: []ControllerLogSelector{
				{Namespace: "capi-system", PodNameRegex: "(unterminated"},
			},
		},
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "pod_name_regex") {
		t.Fatalf("expected pod_name_regex error, got: %v", err)
	}
}

func TestEffectiveEvidence_InvalidBuildLogPatternFailsAtLoad(t *testing.T) {
	c := validConfig()
	c.AI = &AI{
		Module: "capi",
		Evidence: &Evidence{
			BuildLogPatterns: []string{"(unterminated"},
		},
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "build_log_patterns") {
		t.Fatalf("expected build_log_patterns error, got: %v", err)
	}
}

func TestEffectiveEvidence_EmptyNamespaceIsRejected(t *testing.T) {
	c := validConfig()
	c.AI = &AI{
		Module:   "capi",
		Evidence: &Evidence{ControllerLogs: []ControllerLogSelector{{Namespace: "   "}}},
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "namespace is required") {
		t.Fatalf("expected namespace required error, got: %v", err)
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

func TestEvidence_IsZero(t *testing.T) {
	cases := []struct {
		name string
		ev   *Evidence
		want bool
	}{
		{"nil receiver", nil, true},
		{"empty struct", &Evidence{}, true},
		{"machine_logs set", &Evidence{MachineLogs: []string{"foo.log"}}, false},
		{"machine_logs explicit empty slice counts as set",
			&Evidence{MachineLogs: []string{}}, false},
		{"controller_logs set",
			&Evidence{ControllerLogs: []ControllerLogSelector{{Namespace: "x"}}}, false},
		{"build_log_patterns set",
			&Evidence{BuildLogPatterns: []string{"err"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ev.IsZero(); got != tc.want {
				t.Errorf("IsZero() = %v, want %v", got, tc.want)
			}
		})
	}
}
