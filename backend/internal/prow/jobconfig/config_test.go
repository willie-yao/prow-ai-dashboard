package jobconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

const testDashboard = "sig-cluster-lifecycle-cluster-api-provider-azure"

func loadTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading testdata/%s: %v", name, err)
	}
	return data
}

func TestParsePeriodics(t *testing.T) {
	data := loadTestdata(t, "periodics.yaml")
	jobs, err := ParseJobConfig(data, "periodics.yaml", testDashboard, project.DefaultCategories)
	if err != nil {
		t.Fatalf("ParseJobConfig: %v", err)
	}

	// The fixture has 4 jobs; only 3 belong to the CAPZ dashboard.
	if got := len(jobs); got != 3 {
		t.Fatalf("expected 3 jobs, got %d", got)
	}

	// Verify the first job fully.
	j := jobs[0]
	assertEqual(t, "Name", j.Name, "periodic-cluster-api-provider-azure-conformance-main")
	assertEqual(t, "TabName", j.TabName, "capz-periodic-conformance-main")
	assertEqual(t, "Category", j.Category, "conformance")
	assertEqual(t, "Branch", j.Branch, "main")
	assertEqual(t, "Description", j.Description, "Runs conformance & node conformance tests on a CAPZ cluster")
	assertEqual(t, "MinimumInterval", j.MinimumInterval, "48h")
	assertEqual(t, "Timeout", j.Timeout, "4h")
	assertEqual(t, "ConfigFile", j.ConfigFile, "periodics.yaml")

	// Second job — e2e → generic "e2e" category (no project-specific override).
	assertEqual(t, "jobs[1].Category", jobs[1].Category, "e2e")
	assertEqual(t, "jobs[1].MinimumInterval", jobs[1].MinimumInterval, "24h")
	assertEqual(t, "jobs[1].Timeout", jobs[1].Timeout, "3h")

	// Third job — coverage category.
	assertEqual(t, "jobs[2].Category", jobs[2].Category, "coverage")
}

func TestParsePresubmits(t *testing.T) {
	data := loadTestdata(t, "presubmits.yaml")
	jobs, err := ParseJobConfig(data, "presubmits.yaml", testDashboard, project.DefaultCategories)
	if err != nil {
		t.Fatalf("ParseJobConfig: %v", err)
	}

	// 3 jobs in the fixture, 1 is not CAPZ.
	if got := len(jobs); got != 2 {
		t.Fatalf("expected 2 jobs, got %d", got)
	}

	j := jobs[0]
	assertEqual(t, "Name", j.Name, "pull-cluster-api-provider-azure-e2e")
	assertEqual(t, "TabName", j.TabName, "capz-pr-e2e")
	assertEqual(t, "Category", j.Category, "e2e")
	assertEqual(t, "Branch", j.Branch, "main")
	assertEqual(t, "Timeout", j.Timeout, "3h")
	assertEqual(t, "ConfigFile", j.ConfigFile, "presubmits.yaml")

	// Second presubmit — capi-e2e category.
	assertEqual(t, "jobs[1].Category", jobs[1].Category, "capi-e2e")
	assertEqual(t, "jobs[1].Branch", jobs[1].Branch, "release-1.21")
}

func TestCategorize(t *testing.T) {
	// Default rules with no project-specific overrides.
	cases := []struct {
		name     string
		expected string
	}{
		{"periodic-capz-conformance-main", "conformance"},
		{"pull-capz-capi-e2e-main", "capi-e2e"},
		{"periodic-capz-e2e-main", "e2e"},
		{"periodic-capz-upgrade-main", "upgrade"},
		{"periodic-capz-coverage", "coverage"},
		{"periodic-capz-scalability", "scalability"},
		{"periodic-capz-lint", "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := categorize(tc.name, project.DefaultCategories)
			if got != tc.expected {
				t.Errorf("categorize(%q) = %q, want %q", tc.name, got, tc.expected)
			}
		})
	}
}

func TestCategorizeRespectsRuleOrder(t *testing.T) {
	// Consumer-supplied rules take precedence and are evaluated in order.
	rules := []project.CategoryRule{
		{Match: "managed kubernetes", ID: "aks-e2e", Label: "AKS E2E"},
		{Match: "e2e-aks", ID: "aks-e2e", Label: "AKS E2E"},
		{Match: "conformance", ID: "conformance", Label: "Conformance"},
		{Match: "e2e", ID: "capz-e2e", Label: "CAPZ E2E"},
	}
	cases := []struct {
		name, want string
	}{
		// AKS-specific rules win over the generic "e2e" rule.
		{"periodic-capz-e2e-aks-main", "aks-e2e"},
		{"pull-capz-[Managed Kubernetes]-e2e", "aks-e2e"},
		{"periodic-capz-e2e-main", "capz-e2e"},
		{"periodic-capz-conformance-main", "conformance"},
		{"periodic-capz-lint", "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := categorize(tc.name, rules); got != tc.want {
				t.Errorf("categorize(%q) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestSkipJobsForOtherDashboards(t *testing.T) {
	yaml := []byte(`
periodics:
- name: some-other-job
  annotations:
    testgrid-dashboards: sig-other
`)
	jobs, err := ParseJobConfig(yaml, "test.yaml", testDashboard, project.DefaultCategories)
	if err != nil {
		t.Fatalf("ParseJobConfig: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected 0 jobs, got %d", len(jobs))
	}
}

func TestMissingAnnotations(t *testing.T) {
	yaml := []byte(`
periodics:
- name: job-without-annotations
  decoration_config:
    timeout: 1h
`)
	jobs, err := ParseJobConfig(yaml, "test.yaml", testDashboard, project.DefaultCategories)
	if err != nil {
		t.Fatalf("ParseJobConfig: %v", err)
	}
	// No annotations → no testgrid-dashboards → filtered out.
	if len(jobs) != 0 {
		t.Fatalf("expected 0 jobs, got %d", len(jobs))
	}
}

func TestMissingExtraRefs(t *testing.T) {
	yaml := []byte(`
periodics:
- name: capz-job-no-refs
  annotations:
    testgrid-dashboards: sig-cluster-lifecycle-cluster-api-provider-azure
    testgrid-tab-name: capz-no-refs
`)
	jobs, err := ParseJobConfig(yaml, "test.yaml", testDashboard, project.DefaultCategories)
	if err != nil {
		t.Fatalf("ParseJobConfig: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	// Branch should be empty when extra_refs is absent.
	if jobs[0].Branch != "" {
		t.Errorf("expected empty Branch, got %q", jobs[0].Branch)
	}
	if jobs[0].Timeout != "" {
		t.Errorf("expected empty Timeout, got %q", jobs[0].Timeout)
	}
}

func TestEmptyInput(t *testing.T) {
	jobs, err := ParseJobConfig([]byte("{}"), "empty.yaml", testDashboard, project.DefaultCategories)
	if err != nil {
		t.Fatalf("ParseJobConfig: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected 0 jobs, got %d", len(jobs))
	}
}

// CAPI-core-style periodics use interval: instead of minimum_interval:.
// Both forms must produce a non-empty interval so the periodic-only
// filter doesn't drop them. minimum_interval: wins when both are set.
func TestIntervalFallback(t *testing.T) {
	yaml := []byte(`
periodics:
- name: periodic-cluster-api-test-main
  interval: 3h
  annotations:
    testgrid-dashboards: cluster-api-core-main
- name: periodic-mixed-job
  minimum_interval: 24h
  interval: 3h
  annotations:
    testgrid-dashboards: cluster-api-core-main
- name: periodic-cron-only
  annotations:
    testgrid-dashboards: cluster-api-core-main
`)
	jobs, err := ParseJobConfig(yaml, "test.yaml", "cluster-api-core-main", project.DefaultCategories)
	if err != nil {
		t.Fatalf("ParseJobConfig: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("expected 3 jobs, got %d", len(jobs))
	}
	assertEqual(t, "interval-only", jobs[0].MinimumInterval, "3h")
	assertEqual(t, "minimum_interval-wins", jobs[1].MinimumInterval, "24h")
	assertEqual(t, "neither-set", jobs[2].MinimumInterval, "")
}

func assertEqual(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", field, got, want)
	}
}
