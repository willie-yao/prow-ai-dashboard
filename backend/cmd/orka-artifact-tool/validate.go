package main

import (
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
)

func init() {
	registerQTool("/tool/validate_analysis", validateAnalysis)
}

func validateAnalysis(env *toolEnv, w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var args struct {
		Analysis       orka.AnalysisValidation `json:"analysis"`
		EvidenceTokens []string                `json:"evidence_tokens"`
	}
	if err := readArgs(r, &args); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	set, err := skills.ParseHeader(r.Header.Get(skills.ContractHeader))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(args.Analysis.RootCause) == "" {
		http.Error(w, "analysis.root_cause is required", http.StatusBadRequest)
		return
	}
	if env.evidence == nil {
		writeToolError(w, http.StatusInternalServerError, "artifact evidence validation is unavailable")
		return
	}

	readPaths := map[string]bool{}
	invalidTokens := 0
	for _, token := range args.EvidenceTokens {
		path, ok := env.evidence.verify(requestScope(r), strings.TrimSpace(token))
		if !ok {
			invalidTokens++
			continue
		}
		readPaths[path] = true
	}

	present, missing := []string{}, []string{}
	for _, p := range args.Analysis.RelevantFiles {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if readPaths[normalizeEvidencePath(p)] {
			present = append(present, p)
		} else {
			missing = append(missing, p)
		}
	}
	missingEvidence := []string{}
	matchedSkills := set.Match(args.Analysis.EvidenceText())
	for _, skill := range matchedSkills {
		for _, group := range skill.RequiredEvidence {
			if !group.Satisfied(readPaths) {
				missingEvidence = append(missingEvidence, skill.ID+":"+group.ID)
			}
		}
	}
	valid := invalidTokens == 0 && len(missing) == 0 && len(missingEvidence) == 0
	result := map[string]any{
		"checked":                 len(present) + len(missing),
		"present":                 present,
		"missing":                 missing,
		"read_paths":              sortedEvidencePaths(readPaths),
		"invalid_evidence_tokens": invalidTokens,
		"matched_skills":          skillIDs(matchedSkills),
		"missing_evidence":        missingEvidence,
		"all_present":             valid,
	}
	if !valid {
		log.Printf("⚠ validate_analysis paths=%d read=%d missing=%d invalid_tokens=%d evidence_missing=%d", len(args.Analysis.RelevantFiles), len(readPaths), len(missing), invalidTokens, len(missingEvidence))
		writeJSONStatus(w, http.StatusUnprocessableEntity, result)
		return
	}
	result["validation_token"] = orka.AnalysisValidationToken(args.Analysis)
	log.Printf("✔ validate_analysis paths=%d read=%d matched_skills=%d", len(args.Analysis.RelevantFiles), len(readPaths), len(matchedSkills))
	writeJSON(w, result)
}

func normalizeEvidencePath(p string) string {
	p = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(p), "\\", "/"))
	p = strings.TrimPrefix(p, "./")
	return strings.TrimPrefix(p, "/")
}

func sortedEvidencePaths(paths map[string]bool) []string {
	out := make([]string, 0, len(paths))
	for path := range paths {
		out = append(out, path)
	}
	slices.Sort(out)
	return out
}

func skillIDs(matched []skills.Skill) []string {
	ids := make([]string, 0, len(matched))
	for _, skill := range matched {
		ids = append(ids, skill.ID)
	}
	return ids
}
