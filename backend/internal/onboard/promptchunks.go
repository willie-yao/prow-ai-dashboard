package onboard

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	targetPromptExtractionChunkBytes = 12_000
	maxPromptExtractionChunkBytes    = 16_000
	maxPromptChunkEvidenceItems      = 1
	maxPromptChunkMergedItems        = 3
	maxPromptChunkNestedItems        = 4
	maxPromptChunkStringLength       = 300
	maxPromptExtractionAttempts      = 2
	maxPromptUnresolvedItems         = 12
	maxPromptMetadataClaims          = 3
)

type indexedPromptSource struct {
	Index  int
	Source promptSource
}

type promptSourceChunk struct {
	Sources []indexedPromptSource
	Bytes   int
}

func chunkPromptSources(sources []promptSource) ([]promptSourceChunk, error) {
	sorted := append([]promptSource(nil), sources...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Path != sorted[j].Path {
			return sorted[i].Path < sorted[j].Path
		}
		if sorted[i].StartLine != sorted[j].StartLine {
			return sorted[i].StartLine < sorted[j].StartLine
		}
		if sorted[i].EndLine != sorted[j].EndLine {
			return sorted[i].EndLine < sorted[j].EndLine
		}
		if sorted[i].Kind != sorted[j].Kind {
			return sorted[i].Kind < sorted[j].Kind
		}
		return sorted[i].Text < sorted[j].Text
	})

	var chunks []promptSourceChunk
	current := promptSourceChunk{}
	for i, source := range sorted {
		indexed := indexedPromptSource{Index: i + 1, Source: source}
		bytes := len(renderPromptSource(indexed))
		if bytes > maxPromptExtractionChunkBytes {
			return nil, fmt.Errorf("prompt source %q serializes to %d bytes, limit %d", source.Path, bytes, maxPromptExtractionChunkBytes)
		}
		if len(current.Sources) > 0 && current.Bytes+bytes > targetPromptExtractionChunkBytes {
			chunks = append(chunks, current)
			current = promptSourceChunk{}
		}
		current.Sources = append(current.Sources, indexed)
		current.Bytes += bytes
	}
	if len(current.Sources) > 0 {
		chunks = append(chunks, current)
	}
	return chunks, nil
}

func mergePromptEvidence(chunks []promptEvidence) promptEvidence {
	merged := emptyPromptEvidence()
	architecture := map[string]int{}
	lifecycle := map[string]int{}
	flavors := map[string]int{}
	triage := map[string]int{}
	repositories := map[string]int{}
	artifacts := map[string]int{}
	patterns := map[string]int{}
	transients := map[string]int{}
	unresolved := map[string]struct{}{}

	for _, chunk := range chunks {
		mergeEvidenceClaims(&merged.Architecture, architecture, chunk.Architecture)
		mergeEvidenceClaims(&merged.DiagnosticLifecycle, lifecycle, chunk.DiagnosticLifecycle)
		mergeEvidenceClaims(&merged.TestFlavors, flavors, chunk.TestFlavors)
		mergeEvidenceClaims(&merged.TriageOrder, triage, chunk.TriageOrder)
		mergeEvidenceClaims(&merged.Repositories, repositories, chunk.Repositories)
		mergeArtifactEvidence(&merged.Artifacts, artifacts, chunk.Artifacts)
		mergeFailurePatternEvidence(&merged.FailurePatterns, patterns, chunk.FailurePatterns)
		mergeTransientEvidence(&merged.TransientRules, transients, chunk.TransientRules)
		for _, item := range chunk.Unresolved {
			key := normalizedEvidenceKey(item)
			if key == "" || len(merged.Unresolved) >= maxPromptEvidenceItems {
				continue
			}
			if _, ok := unresolved[key]; ok {
				continue
			}
			unresolved[key] = struct{}{}
			merged.Unresolved = append(merged.Unresolved, item)
		}
	}

	trimMergedPromptEvidence(&merged)
	return merged
}

func emptyPromptEvidence() promptEvidence {
	return promptEvidence{
		Architecture:        []evidenceClaim{},
		DiagnosticLifecycle: []evidenceClaim{},
		TestFlavors:         []evidenceClaim{},
		Artifacts:           []artifactEvidence{},
		FailurePatterns:     []failurePatternEvidence{},
		TransientRules:      []transientEvidence{},
		TriageOrder:         []evidenceClaim{},
		Repositories:        []evidenceClaim{},
		Unresolved:          []string{},
	}
}

func mergeEvidenceClaims(dst *[]evidenceClaim, indexes map[string]int, values []evidenceClaim) {
	for _, value := range values {
		key := normalizedEvidenceKey(value.Text)
		if index, ok := indexes[key]; ok {
			(*dst)[index].Sources = mergeEvidenceRefs((*dst)[index].Sources, value.Sources)
			continue
		}
		if key == "" || len(*dst) >= maxPromptEvidenceItems {
			continue
		}
		value.Sources = mergeEvidenceRefs(nil, value.Sources)
		indexes[key] = len(*dst)
		*dst = append(*dst, value)
	}
}

func mergeArtifactEvidence(dst *[]artifactEvidence, indexes map[string]int, values []artifactEvidence) {
	for _, value := range values {
		key := artifactEvidenceKey(value.PathPattern)
		if index, ok := indexes[key]; ok {
			(*dst)[index].Sources = mergeEvidenceRefs((*dst)[index].Sources, value.Sources)
			continue
		}
		if key == "" || len(*dst) >= maxPromptEvidenceItems {
			continue
		}
		value.Sources = mergeEvidenceRefs(nil, value.Sources)
		indexes[key] = len(*dst)
		*dst = append(*dst, value)
	}
}

func mergeFailurePatternEvidence(dst *[]failurePatternEvidence, indexes map[string]int, values []failurePatternEvidence) {
	for _, value := range values {
		key := normalizedEvidenceKey(value.Name)
		if index, ok := indexes[key]; ok {
			(*dst)[index].Sources = mergeEvidenceRefs((*dst)[index].Sources, value.Sources)
			continue
		}
		if key == "" || len(*dst) >= maxPromptEvidenceItems {
			continue
		}
		value.Sources = mergeEvidenceRefs(nil, value.Sources)
		if len(value.RequiredEvidence) > maxPromptEvidenceItems {
			value.RequiredEvidence = value.RequiredEvidence[:maxPromptEvidenceItems]
		}
		indexes[key] = len(*dst)
		*dst = append(*dst, value)
	}
}

func mergeTransientEvidence(dst *[]transientEvidence, indexes map[string]int, values []transientEvidence) {
	for _, value := range values {
		key := normalizedEvidenceKey(value.Class)
		if index, ok := indexes[key]; ok {
			(*dst)[index].Sources = mergeEvidenceRefs((*dst)[index].Sources, value.Sources)
			continue
		}
		if key == "" || len(*dst) >= maxPromptEvidenceItems {
			continue
		}
		value.Sources = mergeEvidenceRefs(nil, value.Sources)
		indexes[key] = len(*dst)
		*dst = append(*dst, value)
	}
}

func mergeEvidenceRefs(existing, additional []evidenceRef) []evidenceRef {
	out := append([]evidenceRef(nil), existing...)
	seen := make(map[string]struct{}, len(out))
	for _, ref := range out {
		seen[evidenceRefKey(ref)] = struct{}{}
	}
	for _, ref := range additional {
		if len(out) >= maxPromptEvidenceItems {
			break
		}
		key := evidenceRefKey(ref)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ref)
	}
	return out
}

func evidenceRefKey(ref evidenceRef) string {
	encoded, _ := json.Marshal(ref)
	return string(encoded)
}

func normalizedEvidenceKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func trimMergedPromptEvidence(e *promptEvidence) {
	for promptEvidenceEncodedSize(*e) > maxPromptEvidenceText {
		switch {
		case len(e.Unresolved) > 0:
			e.Unresolved = e.Unresolved[:len(e.Unresolved)-1]
		case len(e.Repositories) > 0:
			e.Repositories = e.Repositories[:len(e.Repositories)-1]
		case len(e.TriageOrder) > 0:
			e.TriageOrder = e.TriageOrder[:len(e.TriageOrder)-1]
		case len(e.TransientRules) > 0:
			e.TransientRules = e.TransientRules[:len(e.TransientRules)-1]
		case len(e.FailurePatterns) > 0:
			e.FailurePatterns = e.FailurePatterns[:len(e.FailurePatterns)-1]
		case len(e.Artifacts) > 0:
			e.Artifacts = e.Artifacts[:len(e.Artifacts)-1]
		case len(e.TestFlavors) > 0:
			e.TestFlavors = e.TestFlavors[:len(e.TestFlavors)-1]
		case len(e.DiagnosticLifecycle) > 0:
			e.DiagnosticLifecycle = e.DiagnosticLifecycle[:len(e.DiagnosticLifecycle)-1]
		case len(e.Architecture) > 0:
			e.Architecture = e.Architecture[:len(e.Architecture)-1]
		default:
			return
		}
	}
}

func promptEvidenceEncodedSize(e promptEvidence) int {
	encoded, _ := json.Marshal(e)
	return len(encoded)
}

func validatePromptEvidenceRevision(initial, revised promptEvidence) error {
	allowedRefs := map[string]struct{}{}
	for _, ref := range allPromptEvidenceRefs(initial) {
		allowedRefs[evidenceRefKey(ref)] = struct{}{}
	}
	for _, ref := range allPromptEvidenceRefs(revised) {
		if _, ok := allowedRefs[evidenceRefKey(ref)]; !ok {
			return &promptEvidenceValidationError{stage: promptStageStructuredRevision, code: "revision-source", field: "sources"}
		}
	}

	if err := validateRevisedClaims(initial, revised.Architecture, revised.DiagnosticLifecycle, revised.TestFlavors, revised.TriageOrder); err != nil {
		return err
	}
	initialRepositories := evidenceClaimsByKey(initial.Repositories)
	for _, claim := range revised.Repositories {
		matched, ok := initialRepositories[normalizedEvidenceKey(claim.Text)]
		if !ok || !evidenceRefsSubset(claim.Sources, matched.Sources) {
			return revisionContentError("repositories")
		}
	}
	initialArtifacts := artifactsByKey(initial.Artifacts)
	for _, item := range revised.Artifacts {
		matched, ok := initialArtifacts[artifactEvidenceKey(item.PathPattern)]
		if !ok || !evidenceRefsSubset(item.Sources, matched.Sources) || normalizedEvidenceKey(item.Purpose) != normalizedEvidenceKey(matched.Purpose) {
			return revisionContentError("artifacts")
		}
	}
	initialPatterns := failurePatternsByKey(initial.FailurePatterns)
	for _, item := range revised.FailurePatterns {
		matched, ok := initialPatterns[normalizedEvidenceKey(item.Name)]
		if !ok || !evidenceRefsSubset(item.Sources, matched.Sources) {
			return revisionContentError("failure_patterns")
		}
		if normalizedEvidenceKey(item.Signal) != normalizedEvidenceKey(matched.Signal) ||
			normalizedEvidenceKey(item.DoNotConclude) != normalizedEvidenceKey(matched.DoNotConclude) ||
			normalizedEvidenceKey(item.RemediationLimit) != normalizedEvidenceKey(matched.RemediationLimit) ||
			!evidenceStringsSubset(item.RequiredEvidence, matched.RequiredEvidence) {
			return revisionContentError("failure_patterns")
		}
	}
	initialTransients := transientRulesByKey(initial.TransientRules)
	for _, item := range revised.TransientRules {
		matched, ok := initialTransients[normalizedEvidenceKey(item.Class)]
		if !ok || !evidenceRefsSubset(item.Sources, matched.Sources) ||
			normalizedEvidenceKey(item.OnlyIf) != normalizedEvidenceKey(matched.OnlyIf) ||
			normalizedEvidenceKey(item.NotTransientIf) != normalizedEvidenceKey(matched.NotTransientIf) {
			return revisionContentError("transient_rules")
		}
	}
	if !evidenceStringsSubset(revised.Unresolved, initial.Unresolved) {
		return revisionContentError("unresolved")
	}
	return nil
}

func validateRevisedClaims(initial promptEvidence, sections ...[]evidenceClaim) error {
	allowed := map[string]evidenceClaim{}
	appendClaims := func(claims []evidenceClaim) {
		for _, claim := range claims {
			key := normalizedEvidenceKey(claim.Text)
			if existing, ok := allowed[key]; ok {
				existing.Sources = mergeEvidenceRefs(existing.Sources, claim.Sources)
				allowed[key] = existing
				continue
			}
			allowed[key] = claim
		}
	}
	appendClaims(initial.Architecture)
	appendClaims(initial.DiagnosticLifecycle)
	appendClaims(initial.TestFlavors)
	appendClaims(initial.TriageOrder)

	for _, section := range sections {
		for _, claim := range section {
			matched, ok := allowed[normalizedEvidenceKey(claim.Text)]
			if !ok || !evidenceRefsSubset(claim.Sources, matched.Sources) {
				return revisionContentError("claims")
			}
		}
	}
	return nil
}

func revisionContentError(field string) error {
	return &promptEvidenceValidationError{stage: promptStageStructuredRevision, code: "revision-content", field: field}
}

func allPromptEvidenceRefs(e promptEvidence) []evidenceRef {
	var refs []evidenceRef
	appendClaims := func(claims []evidenceClaim) {
		for _, claim := range claims {
			refs = append(refs, claim.Sources...)
		}
	}
	appendClaims(e.Architecture)
	appendClaims(e.DiagnosticLifecycle)
	appendClaims(e.TestFlavors)
	appendClaims(e.TriageOrder)
	appendClaims(e.Repositories)
	for _, item := range e.Artifacts {
		refs = append(refs, item.Sources...)
	}
	for _, item := range e.FailurePatterns {
		refs = append(refs, item.Sources...)
	}
	for _, item := range e.TransientRules {
		refs = append(refs, item.Sources...)
	}
	return refs
}

func evidenceClaimsByKey(values []evidenceClaim) map[string]evidenceClaim {
	out := make(map[string]evidenceClaim, len(values))
	for _, value := range values {
		out[normalizedEvidenceKey(value.Text)] = value
	}
	return out
}

func artifactsByKey(values []artifactEvidence) map[string]artifactEvidence {
	out := make(map[string]artifactEvidence, len(values))
	for _, value := range values {
		out[artifactEvidenceKey(value.PathPattern)] = value
	}
	return out
}

func failurePatternsByKey(values []failurePatternEvidence) map[string]failurePatternEvidence {
	out := make(map[string]failurePatternEvidence, len(values))
	for _, value := range values {
		out[normalizedEvidenceKey(value.Name)] = value
	}
	return out
}

func transientRulesByKey(values []transientEvidence) map[string]transientEvidence {
	out := make(map[string]transientEvidence, len(values))
	for _, value := range values {
		out[normalizedEvidenceKey(value.Class)] = value
	}
	return out
}

func evidenceRefsSubset(values, allowed []evidenceRef) bool {
	allowedKeys := make(map[string]struct{}, len(allowed))
	for _, ref := range allowed {
		allowedKeys[evidenceRefKey(ref)] = struct{}{}
	}
	for _, ref := range values {
		if _, ok := allowedKeys[evidenceRefKey(ref)]; !ok {
			return false
		}
	}
	return true
}

func evidenceStringsSubset(values, allowed []string) bool {
	allowedKeys := make(map[string]struct{}, len(allowed))
	for _, value := range allowed {
		allowedKeys[normalizedEvidenceKey(value)] = struct{}{}
	}
	for _, value := range values {
		if _, ok := allowedKeys[normalizedEvidenceKey(value)]; !ok {
			return false
		}
	}
	return true
}

func artifactEvidenceKey(value string) string {
	return strings.TrimSpace(value)
}
