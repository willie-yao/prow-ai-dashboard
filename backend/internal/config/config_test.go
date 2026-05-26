package config

import (
	"os"
	"path/filepath"
	"testing"
)

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
	jobs, err := ParseJobConfig(data, "periodics.yaml")
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

	// Second job — e2e → capz-e2e category.
	assertEqual(t, "jobs[1].Category", jobs[1].Category, "capz-e2e")
	assertEqual(t, "jobs[1].MinimumInterval", jobs[1].MinimumInterval, "24h")
	assertEqual(t, "jobs[1].Timeout", jobs[1].Timeout, "3h")

	// Third job — coverage category.
	assertEqual(t, "jobs[2].Category", jobs[2].Category, "coverage")
}

func TestParsePresubmits(t *testing.T) {
	data := loadTestdata(t, "presubmits.yaml")
	jobs, err := ParseJobConfig(data, "presubmits.yaml")
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
	assertEqual(t, "Category", j.Category, "capz-e2e")
	assertEqual(t, "Branch", j.Branch, "main")
	assertEqual(t, "Timeout", j.Timeout, "3h")
	assertEqual(t, "ConfigFile", j.ConfigFile, "presubmits.yaml")

	// Second presubmit — capi-e2e category.
	assertEqual(t, "jobs[1].Category", jobs[1].Category, "capi-e2e")
	assertEqual(t, "jobs[1].Branch", jobs[1].Branch, "release-1.21")
}

func TestCategoryInference(t *testing.T) {
	cases := []struct {
		name     string
		expected string
	}{
		{"periodic-capz-conformance-main", "conformance"},
		{"pull-capz-capi-e2e-main", "capi-e2e"},
		{"periodic-capz-e2e-aks-main", "aks-e2e"},
		{"pull-capz-[Managed Kubernetes]-e2e", "aks-e2e"},
		{"periodic-capz-e2e-main", "capz-e2e"},
		{"periodic-capz-upgrade-main", "upgrade"},
		{"periodic-capz-coverage", "coverage"},
		{"periodic-capz-scalability", "scalability"},
		{"periodic-capz-lint", "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := inferCategory(tc.name)
			if got != tc.expected {
				t.Errorf("inferCategory(%q) = %q, want %q", tc.name, got, tc.expected)
			}
		})
	}
}

func TestSkipNonCAPZJobs(t *testing.T) {
	yaml := []byte(`
periodics:
- name: some-other-job
  annotations:
    testgrid-dashboards: sig-other
`)
	jobs, err := ParseJobConfig(yaml, "test.yaml")
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
	jobs, err := ParseJobConfig(yaml, "test.yaml")
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
	jobs, err := ParseJobConfig(yaml, "test.yaml")
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
	jobs, err := ParseJobConfig([]byte("{}"), "empty.yaml")
	if err != nil {
		t.Fatalf("ParseJobConfig: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected 0 jobs, got %d", len(jobs))
	}
}

func assertEqual(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", field, got, want)
	}
}
