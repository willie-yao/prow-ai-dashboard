// Package eval scores the quality of the engine's AI failure analysis against a
// labeled dataset of known failures. It is an offline, on-demand harness, not a
// CI gate: model output is non-deterministic, so the scorecard is a tracked
// measurement (with optional A/B comparison across models, prompts, or config),
// not a pass/fail assertion.
package eval

import (
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Case is one labeled failure in the dataset.
type Case struct {
	// Name uniquely identifies the case.
	Name string `json:"name"`
	// Job, Build, and TestName give context and select the failing test.
	Job      string `json:"job"`
	Build    string `json:"build"`
	TestName string `json:"test_name"`
	// FailureMessage is the JUnit failure text fed to the analyzer.
	FailureMessage string `json:"failure_message"`
	// BuildPrefix is the artifact-tree path (e.g. logs/<job>/<build>/) the
	// analyzer reads, relative to the dataset's artifact root.
	BuildPrefix string `json:"build_prefix"`
	// Labels are the hand-verified ground truth.
	Labels Labels `json:"labels"`
}

// Labels is the ground truth for a case.
type Labels struct {
	// IsTransient is the correct transient-vs-real verdict.
	IsTransient bool `json:"is_transient"`
	// RootCauseKeywords are terms a correct root_cause should mention; scored as
	// recall (case-insensitive substring match).
	RootCauseKeywords []string `json:"root_cause_keywords,omitempty"`
	// ExpectedFiles are artifact/source paths a good analysis should cite.
	ExpectedFiles []string `json:"expected_files,omitempty"`
}

// CaseScore is the per-case scoring of one analysis against its labels.
type CaseScore struct {
	Name string `json:"name"`

	// Available reports whether the analyzer produced a usable result. A failed
	// analysis (broken endpoint, tools-unsupported model) yields an unavailable
	// summary; such cases are excluded from classification metrics so a broken
	// run cannot score as correct.
	Available bool `json:"available"`

	TransientPredicted bool `json:"transient_predicted"`
	TransientExpected  bool `json:"transient_expected"`

	// Grounded reports whether the analysis investigated (made a tool call and
	// fetched evidence) rather than answering blind.
	Grounded bool `json:"grounded"`
	// CitationValidity is the fraction of cited files that actually exist in the
	// build's artifacts (1.0 when nothing was cited).
	CitationValidity float64 `json:"citation_validity"`
	// ExpectedFileRecall is the fraction of labels.expected_files the analysis
	// cited (1.0 when none were expected). Catches an analysis that ignores the
	// evidence files a correct diagnosis should reference.
	ExpectedFileRecall float64 `json:"expected_file_recall"`
	// KeywordRecall is the fraction of expected root-cause keywords present in
	// the analysis root_cause (1.0 when no keywords were specified).
	KeywordRecall float64 `json:"keyword_recall"`

	// Depth signals.
	ToolCalls int `json:"tool_calls"`
	GCSBytes  int `json:"gcs_bytes"`
}

// Score grades one analysis against a case. artifactFiles is the set of file
// paths present in the build's artifacts, used to detect hallucinated citations.
func Score(c Case, summary *models.AISummary, analysis *models.AIAnalysis, artifactFiles map[string]bool) CaseScore {
	s := CaseScore{Name: c.Name, TransientExpected: c.Labels.IsTransient, CitationValidity: 1, ExpectedFileRecall: 1, KeywordRecall: 1}
	// An unavailable analysis (Service.Analyze failed) leaves AIAnalysis nil and
	// must not be scored as a prediction; it is excluded from classification.
	if analysis == nil {
		s.CitationValidity, s.ExpectedFileRecall, s.KeywordRecall = 0, 0, 0
		return s
	}
	s.Available = true
	if summary != nil {
		s.TransientPredicted = summary.IsTransient
	}
	s.ToolCalls = analysis.ToolCalls
	s.GCSBytes = analysis.GCSBytes
	s.Grounded = analysis.ToolCalls >= 1 && analysis.GCSBytes >= 1

	if len(analysis.RelevantFiles) > 0 {
		valid := 0
		for _, f := range analysis.RelevantFiles {
			if citedFileExists(f, artifactFiles) {
				valid++
			}
		}
		s.CitationValidity = float64(valid) / float64(len(analysis.RelevantFiles))
	}

	if exp := c.Labels.ExpectedFiles; len(exp) > 0 {
		cited := citedFileSet(analysis.RelevantFiles)
		found := 0
		for _, want := range exp {
			if cited[basename(want)] {
				found++
			}
		}
		s.ExpectedFileRecall = float64(found) / float64(len(exp))
	}

	if kws := c.Labels.RootCauseKeywords; len(kws) > 0 {
		rc := strings.ToLower(analysis.RootCause)
		found := 0
		for _, kw := range kws {
			if strings.Contains(rc, strings.ToLower(kw)) {
				found++
			}
		}
		s.KeywordRecall = float64(found) / float64(len(kws))
	}
	return s
}

func basename(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// citedFileSet returns the basenames the analysis cited.
func citedFileSet(relevant []string) map[string]bool {
	set := make(map[string]bool, len(relevant))
	for _, f := range relevant {
		set[basename(f)] = true
	}
	return set
}

// citedFileExists reports whether a cited path matches a real artifact. A
// qualified path (containing "/") must match an artifact exactly or as a path
// suffix; a bare filename may match any artifact's basename. This avoids
// crediting a hallucinated qualified path that merely shares a common basename.
func citedFileExists(cited string, artifactFiles map[string]bool) bool {
	cited = strings.TrimSpace(strings.TrimPrefix(cited, "./"))
	if cited == "" {
		return false
	}
	if artifactFiles[cited] {
		return true
	}
	if strings.Contains(cited, "/") {
		suffix := "/" + cited
		for f := range artifactFiles {
			if f == cited || strings.HasSuffix(f, suffix) {
				return true
			}
		}
		return false
	}
	for f := range artifactFiles {
		if basename(f) == cited {
			return true
		}
	}
	return false
}

// Scorecard aggregates per-case scores into dataset-level metrics. Classification
// and quality metrics are computed over AVAILABLE cases only (analyses that
// actually produced a result); Coverage reports the available fraction so a run
// that failed to analyze cannot masquerade as accurate.
type Scorecard struct {
	Cases       int     `json:"cases"`
	Available   int     `json:"available"`
	Unavailable int     `json:"unavailable"`
	Coverage    float64 `json:"coverage"`

	// Transient classification over available cases, treating "real bug"
	// (is_transient=false) as the positive class so precision/recall describe
	// catching real bugs rather than dismissing them as transient.
	TP        int     `json:"tp"`
	FP        int     `json:"fp"`
	TN        int     `json:"tn"`
	FN        int     `json:"fn"`
	Accuracy  float64 `json:"transient_accuracy"`
	Precision float64 `json:"real_bug_precision"`
	Recall    float64 `json:"real_bug_recall"`
	F1        float64 `json:"real_bug_f1"`

	GroundingRate      float64 `json:"grounding_rate"`
	CitationValidity   float64 `json:"citation_validity_mean"`
	ExpectedFileRecall float64 `json:"expected_file_recall_mean"`
	KeywordRecall      float64 `json:"keyword_recall_mean"`
	MeanToolCalls      float64 `json:"mean_tool_calls"`
	MeanGCSBytes       float64 `json:"mean_gcs_bytes"`

	// Meta records what produced this scorecard so A/B comparisons can detect a
	// dataset, model, prompt, or config mismatch.
	Meta   Meta        `json:"meta"`
	Scores []CaseScore `json:"scores,omitempty"`
}

// Meta describes the run that produced a scorecard.
type Meta struct {
	DatasetFingerprint string `json:"dataset_fingerprint,omitempty"`
	Model              string `json:"model,omitempty"`
	PromptFingerprint  string `json:"prompt_fingerprint,omitempty"`
	// AgenticFingerprint hashes the resolved agentic knobs (floors, critique,
	// tools, budgets, single-tool-call, evidence injection) and skill set, so an
	// A/B comparison can detect that the two sides ran under different tuning.
	AgenticFingerprint string `json:"agentic_fingerprint,omitempty"`
	MaxIters           int    `json:"max_iters,omitempty"`
	MinToolCalls       int    `json:"min_tool_calls,omitempty"`
	MinGCSBytes        int    `json:"min_gcs_bytes,omitempty"`
	Critique           bool   `json:"critique,omitempty"`
	Samples            int    `json:"samples,omitempty"`
}

// Aggregate computes a Scorecard from per-case scores. Unavailable cases count
// toward Coverage but are excluded from classification and quality means.
func Aggregate(scores []CaseScore) Scorecard {
	sc := Scorecard{Cases: len(scores), Scores: scores}
	if len(scores) == 0 {
		return sc
	}
	var grounded, citation, expFile, keyword, tools, gcs float64
	correct, avail := 0, 0
	for _, s := range scores {
		if !s.Available {
			continue
		}
		avail++
		// Positive class = real bug (not transient).
		predReal := !s.TransientPredicted
		wantReal := !s.TransientExpected
		switch {
		case predReal && wantReal:
			sc.TP++
		case predReal && !wantReal:
			sc.FP++
		case !predReal && !wantReal:
			sc.TN++
		default:
			sc.FN++
		}
		if s.TransientPredicted == s.TransientExpected {
			correct++
		}
		if s.Grounded {
			grounded++
		}
		citation += s.CitationValidity
		expFile += s.ExpectedFileRecall
		keyword += s.KeywordRecall
		tools += float64(s.ToolCalls)
		gcs += float64(s.GCSBytes)
	}
	sc.Available = avail
	sc.Unavailable = len(scores) - avail
	sc.Coverage = float64(avail) / float64(len(scores))
	if avail == 0 {
		return sc
	}
	n := float64(avail)
	sc.Accuracy = float64(correct) / n
	sc.Precision = ratio(sc.TP, sc.TP+sc.FP)
	sc.Recall = ratio(sc.TP, sc.TP+sc.FN)
	if sc.Precision+sc.Recall > 0 {
		sc.F1 = 2 * sc.Precision * sc.Recall / (sc.Precision + sc.Recall)
	}
	sc.GroundingRate = grounded / n
	sc.CitationValidity = citation / n
	sc.ExpectedFileRecall = expFile / n
	sc.KeywordRecall = keyword / n
	sc.MeanToolCalls = tools / n
	sc.MeanGCSBytes = gcs / n
	return sc
}

func ratio(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// ArtifactFileSet returns the presence set used for citation checks.
func ArtifactFileSet(paths []string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return set
}

// SortScores orders per-case scores by name for stable output.
func SortScores(scores []CaseScore) {
	sort.Slice(scores, func(i, j int) bool { return scores[i].Name < scores[j].Name })
}

// MeanScorecards averages the metrics across samples of the same dataset, so a
// non-deterministic model is summarized by its central tendency rather than one
// arbitrary run. Meta is taken from the first card. Per-case Scores are dropped.
func MeanScorecards(cards []Scorecard) Scorecard {
	if len(cards) == 0 {
		return Scorecard{}
	}
	if len(cards) == 1 {
		return cards[0]
	}
	var out Scorecard
	out.Meta = cards[0].Meta
	out.Cases = cards[0].Cases
	n := float64(len(cards))
	for _, c := range cards {
		out.Available += c.Available
		out.Unavailable += c.Unavailable
		out.TP += c.TP
		out.FP += c.FP
		out.TN += c.TN
		out.FN += c.FN
		out.Coverage += c.Coverage / n
		out.Accuracy += c.Accuracy / n
		out.Precision += c.Precision / n
		out.Recall += c.Recall / n
		out.F1 += c.F1 / n
		out.GroundingRate += c.GroundingRate / n
		out.CitationValidity += c.CitationValidity / n
		out.ExpectedFileRecall += c.ExpectedFileRecall / n
		out.KeywordRecall += c.KeywordRecall / n
		out.MeanToolCalls += c.MeanToolCalls / n
		out.MeanGCSBytes += c.MeanGCSBytes / n
	}
	return out
}
