package eval

import (
	"math"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestScore_TransientAndDepth(t *testing.T) {
	c := Case{Name: "x", Labels: Labels{IsTransient: false}}
	sum := &models.AISummary{IsTransient: false}
	an := &models.AIAnalysis{ToolCalls: 3, GCSBytes: 5000, RootCause: "nil pointer deref", RelevantFiles: []string{"build-log.txt"}}
	files := ArtifactFileSet([]string{"logs/j/1/build-log.txt"})
	s := Score(c, sum, an, files)
	if s.TransientPredicted || s.TransientExpected {
		t.Errorf("transient flags wrong: %+v", s)
	}
	if !s.Grounded {
		t.Error("want grounded (tool calls + bytes)")
	}
	if s.ToolCalls != 3 || s.GCSBytes != 5000 {
		t.Errorf("depth = %d/%d", s.ToolCalls, s.GCSBytes)
	}
}

func TestScore_CitationValidity(t *testing.T) {
	files := ArtifactFileSet([]string{"logs/j/1/build-log.txt", "logs/j/1/artifacts/kubelet.log"})
	c := Case{Name: "x"}
	// One real (by basename), one hallucinated => 0.5.
	an := &models.AIAnalysis{ToolCalls: 1, GCSBytes: 1, RelevantFiles: []string{"build-log.txt", "does-not-exist.log"}}
	s := Score(c, &models.AISummary{}, an, files)
	if !approx(s.CitationValidity, 0.5) {
		t.Errorf("citation validity = %v, want 0.5", s.CitationValidity)
	}
	// No citations => 1.0 (nothing to get wrong).
	an2 := &models.AIAnalysis{RelevantFiles: nil}
	if s2 := Score(c, &models.AISummary{}, an2, files); !approx(s2.CitationValidity, 1) {
		t.Errorf("no-citation validity = %v, want 1", s2.CitationValidity)
	}
}

func TestScore_KeywordRecall(t *testing.T) {
	c := Case{Name: "x", Labels: Labels{RootCauseKeywords: []string{"control plane", "timeout", "etcd"}}}
	an := &models.AIAnalysis{ToolCalls: 1, GCSBytes: 1, RootCause: "The control plane node hit a TIMEOUT during join"}
	s := Score(c, &models.AISummary{}, an, nil)
	if !approx(s.KeywordRecall, 2.0/3.0) {
		t.Errorf("keyword recall = %v, want 2/3", s.KeywordRecall)
	}
}

func TestScore_NilAnalysisScoresZero(t *testing.T) {
	s := Score(Case{Name: "x"}, &models.AISummary{}, nil, nil)
	if s.Available {
		t.Error("nil analysis must be unavailable")
	}
	if s.Grounded || s.CitationValidity != 0 || s.ExpectedFileRecall != 0 || s.KeywordRecall != 0 {
		t.Errorf("nil analysis should score zero, got %+v", s)
	}
}

func TestScore_ExpectedFileRecall(t *testing.T) {
	files := ArtifactFileSet([]string{"logs/j/1/build-log.txt", "logs/j/1/artifacts/kubelet.log"})
	c := Case{Name: "x", Labels: Labels{ExpectedFiles: []string{"build-log.txt", "kubelet.log"}}}
	// Cites one of the two expected files => recall 0.5.
	an := &models.AIAnalysis{ToolCalls: 1, GCSBytes: 1, RelevantFiles: []string{"build-log.txt"}}
	s := Score(c, &models.AISummary{}, an, files)
	if !approx(s.ExpectedFileRecall, 0.5) {
		t.Errorf("expected-file recall = %v, want 0.5", s.ExpectedFileRecall)
	}
	// No expected files => 1.0 (nothing to miss).
	c2 := Case{Name: "y"}
	if s2 := Score(c2, &models.AISummary{}, an, files); !approx(s2.ExpectedFileRecall, 1) {
		t.Errorf("no-expectation recall = %v, want 1", s2.ExpectedFileRecall)
	}
}

func TestAggregate_ConfusionAndMetrics(t *testing.T) {
	// 4 cases. Positive class = real bug (not transient).
	scores := []CaseScore{
		{Available: true, TransientPredicted: false, TransientExpected: false, Grounded: true, CitationValidity: 1, ExpectedFileRecall: 1, KeywordRecall: 1, ToolCalls: 4, GCSBytes: 1000}, // TP
		{Available: true, TransientPredicted: false, TransientExpected: true, Grounded: true, CitationValidity: 0.5, ExpectedFileRecall: 0.5, KeywordRecall: 0.5},                          // FP (called real, was transient)
		{Available: true, TransientPredicted: true, TransientExpected: true, Grounded: false, CitationValidity: 1, ExpectedFileRecall: 1, KeywordRecall: 1},                                // TN
		{Available: true, TransientPredicted: true, TransientExpected: false, Grounded: true, CitationValidity: 1, ExpectedFileRecall: 1, KeywordRecall: 0},                                // FN (called transient, was real)
	}
	sc := Aggregate(scores)
	if sc.Available != 4 || sc.Unavailable != 0 || !approx(sc.Coverage, 1) {
		t.Fatalf("coverage = avail%d unavail%d cov%v", sc.Available, sc.Unavailable, sc.Coverage)
	}
	if sc.TP != 1 || sc.FP != 1 || sc.TN != 1 || sc.FN != 1 {
		t.Fatalf("confusion = TP%d FP%d TN%d FN%d", sc.TP, sc.FP, sc.TN, sc.FN)
	}
	if !approx(sc.Accuracy, 0.5) {
		t.Errorf("accuracy = %v, want 0.5", sc.Accuracy)
	}
	if !approx(sc.Precision, 0.5) || !approx(sc.Recall, 0.5) || !approx(sc.F1, 0.5) {
		t.Errorf("P/R/F1 = %v/%v/%v, want 0.5 each", sc.Precision, sc.Recall, sc.F1)
	}
	if !approx(sc.GroundingRate, 0.75) {
		t.Errorf("grounding = %v, want 0.75", sc.GroundingRate)
	}
	if !approx(sc.CitationValidity, (1+0.5+1+1)/4) {
		t.Errorf("citation mean = %v", sc.CitationValidity)
	}
	if !approx(sc.ExpectedFileRecall, (1+0.5+1+1)/4) {
		t.Errorf("expected-file recall mean = %v", sc.ExpectedFileRecall)
	}
	if !approx(sc.MeanToolCalls, 1.0) {
		t.Errorf("mean tool calls = %v, want 1.0", sc.MeanToolCalls)
	}
}

// TestAggregate_ExcludesUnavailable verifies a failed analysis (Available=false)
// is kept out of the confusion matrix and quality means, but counted in
// coverage, so a broken run cannot masquerade as accurate.
func TestAggregate_ExcludesUnavailable(t *testing.T) {
	scores := []CaseScore{
		{Available: true, TransientPredicted: false, TransientExpected: false, Grounded: true, CitationValidity: 1, ExpectedFileRecall: 1, KeywordRecall: 1}, // TP
		{Available: false, TransientExpected: false}, // analysis failed; must not count as TP
	}
	sc := Aggregate(scores)
	if sc.Available != 1 || sc.Unavailable != 1 || !approx(sc.Coverage, 0.5) {
		t.Fatalf("coverage = avail%d unavail%d cov%v, want 1/1/0.5", sc.Available, sc.Unavailable, sc.Coverage)
	}
	if sc.TP != 1 || sc.FP != 0 || sc.TN != 0 || sc.FN != 0 {
		t.Errorf("confusion = TP%d FP%d TN%d FN%d, unavailable must be excluded", sc.TP, sc.FP, sc.TN, sc.FN)
	}
	if !approx(sc.Accuracy, 1) {
		t.Errorf("accuracy over available = %v, want 1", sc.Accuracy)
	}
}

func TestAggregate_Empty(t *testing.T) {
	if sc := Aggregate(nil); sc.Cases != 0 || sc.F1 != 0 {
		t.Errorf("empty aggregate = %+v", sc)
	}
}

func TestDiffScorecards(t *testing.T) {
	base := Scorecard{F1: 0.4, GroundingRate: 0.5}
	cand := Scorecard{F1: 0.7, GroundingRate: 0.5}
	d := DiffScorecards(base, cand)
	if !approx(d.F1, 0.3) || !approx(d.GroundingRate, 0) {
		t.Errorf("diff = %+v", d)
	}
}

func TestRenderMarkdown_ContainsMetrics(t *testing.T) {
	sc := Aggregate([]CaseScore{{Available: true, TransientPredicted: false, TransientExpected: false, Grounded: true, CitationValidity: 1, ExpectedFileRecall: 1, KeywordRecall: 1}})
	md := RenderMarkdown("Run", sc)
	for _, want := range []string{"Real-bug F1", "Grounding rate", "Expected-file recall", "coverage", "Confusion"} {
		if !contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
}

// TestMeanScorecards_Averages checks N>1 samples collapse to the mean (central
// tendency) rather than one arbitrary run, and that confusion counts sum.
func TestMeanScorecards_Averages(t *testing.T) {
	a := Scorecard{Cases: 2, Available: 2, Coverage: 1, Accuracy: 1, F1: 1, TP: 2, Meta: Meta{Model: "m"}}
	b := Scorecard{Cases: 2, Available: 2, Coverage: 1, Accuracy: 0, F1: 0, TP: 0, Meta: Meta{Model: "m"}}
	mean := MeanScorecards([]Scorecard{a, b})
	if !approx(mean.Accuracy, 0.5) || !approx(mean.F1, 0.5) {
		t.Errorf("mean accuracy/F1 = %v/%v, want 0.5", mean.Accuracy, mean.F1)
	}
	if mean.TP != 2 {
		t.Errorf("summed TP = %d, want 2", mean.TP)
	}
	if mean.Meta.Model != "m" {
		t.Errorf("meta not carried from first card: %+v", mean.Meta)
	}
	// A single card is returned unchanged (keeps per-case detail).
	if got := MeanScorecards([]Scorecard{a}); got.Accuracy != 1 {
		t.Errorf("single-card mean = %v, want passthrough 1", got.Accuracy)
	}
}

// TestMetaMismatch flags A/B comparisons computed under different conditions.
func TestMetaMismatch(t *testing.T) {
	base := Scorecard{Meta: Meta{DatasetFingerprint: "aaa", AgenticFingerprint: "cfg1", Samples: 5}}
	same := Scorecard{Meta: Meta{DatasetFingerprint: "aaa", AgenticFingerprint: "cfg1", Samples: 5}}
	if w := MetaMismatch(base, same); w != "" {
		t.Errorf("identical meta should not warn, got %q", w)
	}
	// Differing model alone must NOT warn: comparing models is the point of A/B.
	model := Scorecard{Meta: Meta{DatasetFingerprint: "aaa", AgenticFingerprint: "cfg1", Samples: 5, Model: "other"}}
	if w := MetaMismatch(base, model); w != "" {
		t.Errorf("model difference must not warn, got %q", w)
	}
	// Differing dataset, agentic config, or sample count must warn.
	for _, cand := range []Scorecard{
		{Meta: Meta{DatasetFingerprint: "bbb", AgenticFingerprint: "cfg1", Samples: 5}},
		{Meta: Meta{DatasetFingerprint: "aaa", AgenticFingerprint: "cfg2", Samples: 5}},
		{Meta: Meta{DatasetFingerprint: "aaa", AgenticFingerprint: "cfg1", Samples: 1}},
	} {
		if MetaMismatch(base, cand) == "" {
			t.Errorf("expected mismatch warning for %+v", cand.Meta)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
