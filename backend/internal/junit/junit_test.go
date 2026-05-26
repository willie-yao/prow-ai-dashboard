package junit

import (
	"os"
	"strings"
	"testing"
)

func loadFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/junit.xml")
	if err != nil {
		t.Fatalf("failed to read fixture: %v", err)
	}
	return data
}

func TestParse(t *testing.T) {
	data := loadFixture(t)
	cases, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	if got := len(cases); got != 5 {
		t.Fatalf("expected 5 test cases, got %d", got)
	}

	// Failed test
	tc := cases[0]
	if tc.Status != "failed" {
		t.Errorf("expected status 'failed', got %q", tc.Status)
	}
	if tc.DurationSeconds != 4600.00 {
		t.Errorf("expected duration 4600.00, got %f", tc.DurationSeconds)
	}
	if !strings.Contains(tc.Name, "HA cluster") {
		t.Errorf("unexpected name: %s", tc.Name)
	}
	if !strings.Contains(tc.FailureMessage, "Timed out after 3600.001s") {
		t.Errorf("failure message missing expected text: %s", tc.FailureMessage)
	}
	if !strings.Contains(tc.FailureBody, "Full Stack Trace") {
		t.Errorf("failure body missing stack trace: %s", tc.FailureBody)
	}
	if tc.FailureLocation == "" {
		t.Error("expected failure location to be extracted")
	}
	if tc.FailureLocURL == "" {
		t.Error("expected failure location URL to be generated")
	}

	// Skipped test (via <skipped> element)
	tc = cases[1]
	if tc.Status != "skipped" {
		t.Errorf("expected status 'skipped', got %q", tc.Status)
	}

	// Passed test
	tc = cases[2]
	if tc.Status != "passed" {
		t.Errorf("expected status 'passed', got %q", tc.Status)
	}
	if tc.DurationSeconds != 1234.56 {
		t.Errorf("expected duration 1234.56, got %f", tc.DurationSeconds)
	}

	// Skipped test (via status attr only, no <skipped> element)
	tc = cases[3]
	if tc.Status != "skipped" {
		t.Errorf("expected status 'skipped', got %q", tc.Status)
	}

	// Second suite
	tc = cases[4]
	if tc.Status != "passed" {
		t.Errorf("expected status 'passed', got %q", tc.Status)
	}
	if tc.DurationSeconds != 300.00 {
		t.Errorf("expected duration 300.00, got %f", tc.DurationSeconds)
	}
}

func TestExtractFailureLocation_Versioned(t *testing.T) {
	body := `[FAILED] some error
sigs.k8s.io/cluster-api/test@v1.12.3/framework/controlplane_helpers.go:115
sigs.k8s.io/cluster-api-provider-azure/test/e2e/helpers.go:42`

	loc, url := ExtractFailureLocation(body)

	expectedLoc := "sigs.k8s.io/cluster-api/test@v1.12.3/framework/controlplane_helpers.go:115"
	if loc != expectedLoc {
		t.Errorf("location = %q, want %q", loc, expectedLoc)
	}

	expectedURL := "https://github.com/kubernetes-sigs/cluster-api/blob/v1.12.3/test/framework/controlplane_helpers.go#L115"
	if url != expectedURL {
		t.Errorf("url = %q, want %q", url, expectedURL)
	}
}

func TestExtractFailureLocation_Unversioned(t *testing.T) {
	body := `sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:42`

	loc, url := ExtractFailureLocation(body)

	expectedLoc := "sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:42"
	if loc != expectedLoc {
		t.Errorf("location = %q, want %q", loc, expectedLoc)
	}

	expectedURL := "https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/main/test/e2e/azure_test.go#L42"
	if url != expectedURL {
		t.Errorf("url = %q, want %q", url, expectedURL)
	}
}

func TestExtractFailureLocation_MultipleLocations(t *testing.T) {
	body := `first location:
sigs.k8s.io/cluster-api/test@v1.12.3/framework/controlplane_helpers.go:115
second location:
sigs.k8s.io/cluster-api-provider-azure/test/e2e/azure_test.go:42`

	loc, _ := ExtractFailureLocation(body)
	if !strings.Contains(loc, "controlplane_helpers.go:115") {
		t.Errorf("expected first location to be extracted, got %q", loc)
	}
}

func TestExtractFailureLocation_NoMatch(t *testing.T) {
	loc, url := ExtractFailureLocation("no go source references here")
	if loc != "" || url != "" {
		t.Errorf("expected empty results, got loc=%q url=%q", loc, url)
	}
}

func TestParseSummary(t *testing.T) {
	data := loadFixture(t)
	total, passed, failed, skipped, err := ParseSummary(data)
	if err != nil {
		t.Fatalf("ParseSummary returned error: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if passed != 2 {
		t.Errorf("passed = %d, want 2", passed)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2", skipped)
	}
}

func TestParse_EmptyXML(t *testing.T) {
	_, err := Parse([]byte(""))
	if err == nil {
		t.Error("expected error for empty XML")
	}
}

func TestParse_MalformedXML(t *testing.T) {
	_, err := Parse([]byte("<testsuites><broken"))
	if err == nil {
		t.Error("expected error for malformed XML")
	}
}

func TestParse_SingleTestSuite(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="suite1" tests="1" failures="0" errors="0" time="10.0">
  <testcase name="test1" classname="suite1" status="passed" time="10.0"/>
</testsuite>`

	cases, err := Parse([]byte(xml))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if len(cases) != 1 {
		t.Fatalf("expected 1 test case, got %d", len(cases))
	}
	if cases[0].Status != "passed" {
		t.Errorf("expected passed, got %q", cases[0].Status)
	}
}
