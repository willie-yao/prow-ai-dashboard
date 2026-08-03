package ai

import (
	"slices"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestAnalysisCitationValidation(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{
		"build-log.txt": {
			Segments: []string{"error: error execution phase etcd-join"},
			Lines:    map[int]string{2494: "error:  error execution phase etcd-join"},
		},
	}
	valid := analysisResponse{
		Summary: "etcd join failed", RootCause: "The etcd join failure is shown at line 2494.",
		SuggestedFix: "Correct the etcd join configuration and rerun the job.", Severity: "High",
		EvidenceCitations: []models.EvidenceCitation{{
			Path: "build-log.txt", LineStart: 2494, LineEnd: 2494,
			Quote: "error: error execution phase etcd-join",
		}},
	}
	out := critiqueDraftWithContent(valid, nil, nil, nil, nil, nil, 0, analysisCitationContext{Evidence: evidence})
	if !out.Passed || len(out.CitationIssues) != 0 {
		t.Fatalf("valid citation outcome = %+v", out)
	}

	wrongLine := valid
	wrongLine.RootCause = "The etcd join failure is shown at line 73."
	out = critiqueDraftWithContent(wrongLine, nil, nil, nil, nil, nil, 0, analysisCitationContext{Evidence: evidence})
	if out.Passed || !strings.Contains(strings.Join(out.CitationIssues, " "), "line claim 73-73") {
		t.Fatalf("wrong-line outcome = %+v", out)
	}
	sanitized := sanitizePublishedCitations(wrongLine, analysisCitationContext{Evidence: evidence})
	if strings.Contains(sanitized.RootCause, "line 73") || len(sanitized.EvidenceCitations) != 1 {
		t.Fatalf("sanitized wrong-line analysis = %+v", sanitized)
	}

	wrongPath := valid
	wrongPath.RootCause = "The etcd join failure is shown in other.log at line 2494."
	out = critiqueDraftWithContent(wrongPath, nil, nil, nil, nil, nil, 0, analysisCitationContext{Evidence: evidence})
	if out.Passed || !strings.Contains(strings.Join(out.CitationIssues, " "), "other.log:2494-2494") {
		t.Fatalf("wrong-path outcome = %+v", out)
	}
	sanitized = sanitizePublishedCitations(wrongPath, analysisCitationContext{Evidence: evidence})
	if strings.Contains(sanitized.RootCause, "line 2494") {
		t.Fatalf("wrong-path line claim was published: %+v", sanitized)
	}
}

func TestKernelTimestampIsNotLineClaim(t *testing.T) {
	parsed := analysisResponse{
		Summary: "kernel delay", RootCause: "The kernel timestamp 73.123 seconds precedes the timeout.",
		SuggestedFix: "Correct the timeout configuration and rerun the job.", Severity: "Medium",
	}
	out := critiqueDraftWithContent(parsed, nil, nil, nil, nil, nil, 0, analysisCitationContext{Evidence: map[string]*analysisChatEvidence{}})
	if !out.Passed || len(out.CitationIssues) != 0 {
		t.Fatalf("timestamp outcome = %+v", out)
	}
}

func TestPreparePublishedAnalysisFiltersPathsAndCLIFlags(t *testing.T) {
	state := &agentState{
		readArtifactsFull: map[string]bool{"config/observed.yaml": true},
		readArtifactsBase: map[string]bool{"observed.yaml": true},
		readSourceFull: map[string]bool{
			"scripts/ci-e2e.sh": true,
			"makefile":          true,
		},
		sourceContentByPath: map[string][]string{
			"makefile": {"ci-e2e: ./scripts/ci-e2e.sh"},
		},
	}
	parsed := analysisResponse{
		RootCause: "AKS lifecycle configuration using --enable-long-term-support caused the build failure.", Severity: "High",
		SuggestedFix:  "Update config/observed.yaml and scripts/aks-create.sh, then run az aks update --enable-long-term-support --lts-version 1.2 and rerun the job.",
		RelevantFiles: []string{"build-log.txt", "scripts/aks-create.sh", "scripts/ci-e2e.sh", "Makefile"},
	}
	got := state.preparePublishedAnalysis(parsed)
	if !slices.Equal(got.RelevantFiles, []string{"scripts/ci-e2e.sh", "Makefile"}) {
		t.Fatalf("relevant files = %v", got.RelevantFiles)
	}
	if !slices.Contains(got.SearchSuggestions, "scripts/aks-create.sh") {
		t.Fatalf("search suggestions = %v", got.SearchSuggestions)
	}
	if slices.Contains(got.SearchSuggestions, "build-log.txt") {
		t.Fatalf("artifact path became a source search suggestion: %v", got.SearchSuggestions)
	}
	if strings.Contains(got.SuggestedFix, "config/observed.yaml") || strings.Contains(got.SuggestedFix, "scripts/aks-create.sh") || strings.Contains(got.SuggestedFix, "--enable-") || strings.Contains(got.SuggestedFix, "--lts-") {
		t.Fatalf("unsupported remediation leaked: %q", got.SuggestedFix)
	}
	if strings.Contains(got.RootCause, "--enable-long-term-support") || !strings.Contains(got.RootCause, "AKS lifecycle configuration") {
		t.Fatalf("root cause was not safely preserved: %q", got.RootCause)
	}
}

func TestPreparePublishedAnalysisKeepsGroundedFlag(t *testing.T) {
	state := &agentState{sourceContentByPath: map[string][]string{"Makefile": {"tool --supported"}}}
	parsed := analysisResponse{SuggestedFix: "Run tool --supported and rerun the job."}
	if got := state.preparePublishedAnalysis(parsed).SuggestedFix; !strings.Contains(got, "--supported") {
		t.Fatalf("grounded flag removed: %q", got)
	}
}

func TestPreparePublishedAnalysisRequiresExactFlagGrounding(t *testing.T) {
	state := &agentState{sourceContentByPath: map[string][]string{"Makefile": {"tool --supported-extra"}}}
	parsed := analysisResponse{SuggestedFix: "Run tool --supported and rerun the job."}
	if got := state.preparePublishedAnalysis(parsed).SuggestedFix; strings.Contains(got, "--supported") {
		t.Fatalf("substring-grounded flag survived: %q", got)
	}
}

func TestRecordSourceContentFromVisibleGrepPayload(t *testing.T) {
	state := &agentState{}
	state.recordSourceContent(modelToolCall{Function: modelFunction{Name: "grep_repo"}}, map[string]interface{}{
		"matches": []interface{}{map[string]interface{}{
			"path": "Makefile", "context": []interface{}{"> 12: tool --supported"},
		}},
	})
	if got := state.preparePublishedAnalysis(analysisResponse{SuggestedFix: "Run tool --supported and rerun the job."}).SuggestedFix; !strings.Contains(got, "--supported") {
		t.Fatalf("visible grep grounding was not recorded: %q", got)
	}
}

func TestResponseFormatIncludesEvidenceContract(t *testing.T) {
	for _, required := range []string{"evidence_citations", "line_start", "search_suggestions", "verified source files", "exact CLI flags"} {
		if !strings.Contains(ResponseFormatFooter, required) {
			t.Fatalf("ResponseFormatFooter missing %q", required)
		}
	}
}

func TestInvalidEvidenceCitationIsDropped(t *testing.T) {
	parsed := analysisResponse{
		RootCause:         "The failure is shown at line 10.",
		EvidenceCitations: []models.EvidenceCitation{{Path: "unread.log", LineStart: 10, LineEnd: 10, Quote: "error text"}},
	}
	context := analysisCitationContext{Evidence: map[string]*analysisChatEvidence{}}
	out := critiqueDraftWithContent(parsed, nil, nil, nil, nil, nil, 0, context)
	if out.Passed || len(out.CitationIssues) == 0 {
		t.Fatalf("invalid citation outcome = %+v", out)
	}
	sanitized := sanitizePublishedCitations(parsed, context)
	if len(sanitized.EvidenceCitations) != 0 || strings.Contains(sanitized.RootCause, "line 10") {
		t.Fatalf("invalid citation was published: %+v", sanitized)
	}
}

func TestPublishedEvidenceCitationsAreBounded(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{"build-log.txt": {Lines: map[int]string{1: "error text"}}}
	parsed := analysisResponse{}
	for range 21 {
		parsed.EvidenceCitations = append(parsed.EvidenceCitations, models.EvidenceCitation{
			Path: "build-log.txt", LineStart: 1, LineEnd: 1, Quote: "error text",
		})
	}
	sanitized := sanitizePublishedCitations(parsed, analysisCitationContext{Evidence: evidence})
	if len(sanitized.EvidenceCitations) != 20 {
		t.Fatalf("published citations = %d, want 20", len(sanitized.EvidenceCitations))
	}
}
