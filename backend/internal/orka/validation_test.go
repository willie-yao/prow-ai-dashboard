package orka

import "testing"

func TestAnalysisValidationTokenTracksFinalFields(t *testing.T) {
	base := AnalysisValidation{Summary: "summary", RootCause: "cause", Severity: "High", SuggestedFix: "fix", RelevantFiles: []string{"b", "a"}}
	first := AnalysisValidationToken(base)
	reordered := base
	reordered.RelevantFiles = []string{"a", "b"}
	if AnalysisValidationToken(reordered) != first {
		t.Fatal("file ordering changed validation token")
	}
	changed := base
	changed.RootCause = "other"
	if AnalysisValidationToken(changed) == first {
		t.Fatal("root cause change did not change validation token")
	}
}
