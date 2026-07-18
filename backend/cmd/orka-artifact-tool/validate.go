package main

import (
	"log"
	"net/http"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
)

func init() {
	registerQTool("/tool/validate_analysis", validateAnalysis)
}

func validateAnalysis(env *toolEnv, w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var args struct {
		Paths    []string `json:"paths"`
		Analysis string   `json:"analysis"`
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
	if set.Hash() != "" && strings.TrimSpace(args.Analysis) == "" {
		http.Error(w, "analysis is required when consumer skills are configured", http.StatusBadRequest)
		return
	}
	ctx, cancel := requestCtx(r)
	defer cancel()

	present, missing := []string{}, []string{}
	validatedPaths := map[string]bool{}
	for _, p := range args.Paths {
		if p == "" {
			continue
		}
		if _, _, err := env.browser.Read(ctx, p, 0, 1); err != nil {
			missing = append(missing, p)
		} else {
			present = append(present, p)
			validatedPaths[normalizeEvidencePath(p)] = true
		}
	}
	missingEvidence := []string{}
	matchedSkills := set.Match(args.Analysis)
	for _, skill := range matchedSkills {
		for _, group := range skill.RequiredEvidence {
			if !group.Satisfied(validatedPaths) {
				missingEvidence = append(missingEvidence, skill.ID+":"+group.ID)
			}
		}
	}
	result := map[string]any{
		"checked":          len(present) + len(missing),
		"present":          present,
		"missing":          missing,
		"matched_skills":   skillIDs(matchedSkills),
		"missing_evidence": missingEvidence,
		"all_present":      len(missing) == 0 && len(missingEvidence) == 0,
	}
	if len(missing) > 0 || len(missingEvidence) > 0 {
		log.Printf("⚠ validate_analysis paths=%d present=%d missing=%d evidence_missing=%d", len(args.Paths), len(present), len(missing), len(missingEvidence))
		writeJSONStatus(w, http.StatusUnprocessableEntity, result)
		return
	}
	log.Printf("✔ validate_analysis paths=%d present=%d matched_skills=%d", len(args.Paths), len(present), len(matchedSkills))
	writeJSON(w, result)
}

func normalizeEvidencePath(p string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(p), "\\", "/"))
}

func skillIDs(matched []skills.Skill) []string {
	ids := make([]string, 0, len(matched))
	for _, skill := range matched {
		ids = append(ids, skill.ID)
	}
	return ids
}
