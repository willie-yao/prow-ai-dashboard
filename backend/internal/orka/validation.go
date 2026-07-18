package orka

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// AnalysisValidation is the exact final analysis validated by validate_analysis.
type AnalysisValidation struct {
	Summary       string   `json:"summary"`
	RootCause     string   `json:"root_cause"`
	Severity      string   `json:"severity"`
	IsTransient   bool     `json:"is_transient"`
	SuggestedFix  string   `json:"suggested_fix"`
	RelevantFiles []string `json:"relevant_files"`
}

// EvidenceText is the consumer-skill trigger input for this analysis.
func (a AnalysisValidation) EvidenceText() string {
	return strings.Join([]string{
		a.Summary,
		a.RootCause,
		a.Severity,
		a.SuggestedFix,
		strings.Join(a.RelevantFiles, "\n"),
	}, "\n")
}

// AnalysisValidationToken fingerprints the canonical validated result.
func AnalysisValidationToken(a AnalysisValidation) string {
	a.Summary = strings.TrimSpace(a.Summary)
	a.RootCause = strings.TrimSpace(a.RootCause)
	a.Severity = strings.TrimSpace(a.Severity)
	a.SuggestedFix = strings.TrimSpace(a.SuggestedFix)
	files := append([]string(nil), a.RelevantFiles...)
	for i := range files {
		files[i] = strings.TrimSpace(files[i])
	}
	sort.Strings(files)
	a.RelevantFiles = files
	data, _ := json.Marshal(a)
	sum := sha256.Sum256(append([]byte("orka-analysis-validation-v1\x00"), data...))
	return hex.EncodeToString(sum[:])
}
