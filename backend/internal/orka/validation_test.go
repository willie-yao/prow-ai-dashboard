package orka

import "testing"

func TestAnalysisValidationTokenTracksKeyAndFinalFields(t *testing.T) {
	base := AnalysisValidation{Summary: "summary", RootCause: "cause", Severity: "High", SuggestedFix: "fix", RelevantFiles: []string{"b", "a"}}
	first := AnalysisValidationToken("key-a", base)
	reordered := base
	reordered.RelevantFiles = []string{"a", "b"}
	if AnalysisValidationToken("key-a", reordered) != first {
		t.Fatal("reordering relevant files changed token")
	}
	changed := base
	changed.RootCause = "different"
	if AnalysisValidationToken("key-a", changed) == first {
		t.Fatal("changed final field did not change token")
	}
	if AnalysisValidationToken("key-b", base) == first {
		t.Fatal("different validation key produced the same token")
	}
}
