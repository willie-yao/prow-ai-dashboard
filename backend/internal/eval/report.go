package eval

import (
	"fmt"
	"strings"
)

// Diff is a per-metric A/B comparison between a baseline and a candidate
// scorecard (candidate minus baseline).
type Diff struct {
	Coverage           float64 `json:"coverage"`
	Accuracy           float64 `json:"transient_accuracy"`
	Precision          float64 `json:"real_bug_precision"`
	Recall             float64 `json:"real_bug_recall"`
	F1                 float64 `json:"real_bug_f1"`
	GroundingRate      float64 `json:"grounding_rate"`
	CitationValidity   float64 `json:"citation_validity_mean"`
	ExpectedFileRecall float64 `json:"expected_file_recall_mean"`
	KeywordRecall      float64 `json:"keyword_recall_mean"`
	MeanToolCalls      float64 `json:"mean_tool_calls"`
	MeanGCSBytes       float64 `json:"mean_gcs_bytes"`
}

// DiffScorecards returns candidate minus baseline for each metric.
func DiffScorecards(baseline, candidate Scorecard) Diff {
	return Diff{
		Coverage:           candidate.Coverage - baseline.Coverage,
		Accuracy:           candidate.Accuracy - baseline.Accuracy,
		Precision:          candidate.Precision - baseline.Precision,
		Recall:             candidate.Recall - baseline.Recall,
		F1:                 candidate.F1 - baseline.F1,
		GroundingRate:      candidate.GroundingRate - baseline.GroundingRate,
		CitationValidity:   candidate.CitationValidity - baseline.CitationValidity,
		ExpectedFileRecall: candidate.ExpectedFileRecall - baseline.ExpectedFileRecall,
		KeywordRecall:      candidate.KeywordRecall - baseline.KeywordRecall,
		MeanToolCalls:      candidate.MeanToolCalls - baseline.MeanToolCalls,
		MeanGCSBytes:       candidate.MeanGCSBytes - baseline.MeanGCSBytes,
	}
}

// RenderMarkdown formats a scorecard as a human-readable summary.
func RenderMarkdown(title string, sc Scorecard) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "Cases: %d (available %d, unavailable %d, coverage %.2f)\n\n", sc.Cases, sc.Available, sc.Unavailable, sc.Coverage)
	if sc.Meta.Model != "" {
		fmt.Fprintf(&b, "Model: `%s` · dataset `%s` · prompt `%s` · max_iters %d · floors %d/%d · critique %v · samples %d\n\n",
			sc.Meta.Model, sc.Meta.DatasetFingerprint, sc.Meta.PromptFingerprint, sc.Meta.MaxIters, sc.Meta.MinToolCalls, sc.Meta.MinGCSBytes, sc.Meta.Critique, sc.Meta.Samples)
	}
	fmt.Fprintf(&b, "| Metric | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| Transient accuracy | %.2f |\n", sc.Accuracy)
	fmt.Fprintf(&b, "| Real-bug precision | %.2f |\n", sc.Precision)
	fmt.Fprintf(&b, "| Real-bug recall | %.2f |\n", sc.Recall)
	fmt.Fprintf(&b, "| Real-bug F1 | %.2f |\n", sc.F1)
	fmt.Fprintf(&b, "| Grounding rate | %.2f |\n", sc.GroundingRate)
	fmt.Fprintf(&b, "| Citation validity | %.2f |\n", sc.CitationValidity)
	fmt.Fprintf(&b, "| Expected-file recall | %.2f |\n", sc.ExpectedFileRecall)
	fmt.Fprintf(&b, "| Keyword recall | %.2f |\n", sc.KeywordRecall)
	fmt.Fprintf(&b, "| Mean tool calls | %.1f |\n", sc.MeanToolCalls)
	fmt.Fprintf(&b, "| Mean GCS KB | %.0f |\n", sc.MeanGCSBytes/1024)
	fmt.Fprintf(&b, "\nConfusion (positive = real bug): TP=%d FP=%d TN=%d FN=%d\n", sc.TP, sc.FP, sc.TN, sc.FN)
	return b.String()
}

// MetaMismatch returns a human-readable warning when two scorecards were
// produced under conditions that make an A/B comparison unsound: a different
// dataset/evidence, prompt, agentic tuning, or sample count. The model is
// intentionally not flagged, since comparing models is the primary use of A/B;
// it is shown in each scorecard's meta line for context instead. Empty when the
// two are comparable.
func MetaMismatch(base, cand Scorecard) string {
	var diffs []string
	if base.Meta.DatasetFingerprint != cand.Meta.DatasetFingerprint {
		diffs = append(diffs, fmt.Sprintf("dataset/evidence (%s vs %s)", base.Meta.DatasetFingerprint, cand.Meta.DatasetFingerprint))
	}
	if base.Meta.PromptFingerprint != cand.Meta.PromptFingerprint {
		diffs = append(diffs, "prompt")
	}
	if base.Meta.AgenticFingerprint != cand.Meta.AgenticFingerprint {
		diffs = append(diffs, "agentic config (floors/critique/tools/budgets/skills)")
	}
	if base.Meta.Samples != cand.Meta.Samples {
		diffs = append(diffs, fmt.Sprintf("sample count (%d vs %d)", base.Meta.Samples, cand.Meta.Samples))
	}
	if len(diffs) == 0 {
		return ""
	}
	return "⚠ baseline and candidate differ in " + strings.Join(diffs, ", ") + "; the comparison may not be apples-to-apples."
}

// RenderDiffMarkdown formats an A/B comparison with per-metric deltas.
func RenderDiffMarkdown(baseTitle, candTitle string, base, cand Scorecard) string {
	d := DiffScorecards(base, cand)
	var b strings.Builder
	fmt.Fprintf(&b, "# A/B: %s vs %s\n\n", candTitle, baseTitle)
	fmt.Fprintf(&b, "Cases: %d · coverage %.2f (avail %d) vs %.2f (avail %d)\n\n",
		cand.Cases, base.Coverage, base.Available, cand.Coverage, cand.Available)
	fmt.Fprintf(&b, "| Metric | %s | %s | Δ |\n|---|---|---|---|\n", baseTitle, candTitle)
	row := func(name string, b0, c0, delta float64) {
		fmt.Fprintf(&b, "| %s | %.2f | %.2f | %+.2f |\n", name, b0, c0, delta)
	}
	row("Coverage", base.Coverage, cand.Coverage, d.Coverage)
	row("Transient accuracy", base.Accuracy, cand.Accuracy, d.Accuracy)
	row("Real-bug precision", base.Precision, cand.Precision, d.Precision)
	row("Real-bug recall", base.Recall, cand.Recall, d.Recall)
	row("Real-bug F1", base.F1, cand.F1, d.F1)
	row("Grounding rate", base.GroundingRate, cand.GroundingRate, d.GroundingRate)
	row("Citation validity", base.CitationValidity, cand.CitationValidity, d.CitationValidity)
	row("Expected-file recall", base.ExpectedFileRecall, cand.ExpectedFileRecall, d.ExpectedFileRecall)
	row("Keyword recall", base.KeywordRecall, cand.KeywordRecall, d.KeywordRecall)
	row("Mean tool calls", base.MeanToolCalls, cand.MeanToolCalls, d.MeanToolCalls)
	if w := MetaMismatch(base, cand); w != "" {
		fmt.Fprintf(&b, "\n%s\n", w)
	}
	return b.String()
}
