package causalcritic

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
)

const (
	DigestSchemaVersion  = 1
	DigestTargetBytes    = 8 << 10
	DigestHardLimitBytes = 16 << 10

	InputArmFullBundle = "full_bundle"
	InputArmDigestV1   = "digest_v1"
)

const (
	DigestCategoryCitation        = "citation"
	DigestCategoryCitationContext = "citation_context"
	DigestCategorySpecificError   = "specific_error"
	DigestCategoryTimeline        = "timeline"
	DigestCategorySuccess         = "success_counterevidence"
	DigestCategoryOwnership       = "ownership"
	DigestCategoryContext         = "context"
)

var (
	digestTimelineRE  = regexp.MustCompile(`(?i)(\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}|\b\d{2}:\d{2}:\d{2}\b|\b(created|started|failed|error|retry|deleted|ready|reconciled|admitted|scheduled|bound|provisioned)\b)`)
	digestOwnershipRE = regexp.MustCompile(`(?i)\b(controller|operator|webhook|admission|reconcile|owner|managed by|serviceaccount|namespace|pod|node|deployment|statefulset|daemonset)\b`)
	digestGrepLineRE  = regexp.MustCompile(`^(?:>\s*)?([0-9]+):\s?(.*)$`)
)

// DigestLine retains one model-visible line and its original frozen provenance.
type DigestLine struct {
	Category  string            `json:"category"`
	Reference EvidenceReference `json:"reference"`
	Text      string            `json:"text"`
	Truncated bool              `json:"truncated,omitempty"`
}

// DigestProvenanceLine retains private original-to-compact reference mapping.
type DigestProvenanceLine struct {
	Category        string            `json:"category"`
	SourceReference EvidenceReference `json:"source_reference"`
	Reference       EvidenceReference `json:"reference"`
	Truncated       bool              `json:"truncated,omitempty"`
}

// DigestOmissions reports evidence excluded by the deterministic selector.
type DigestOmissions struct {
	Excerpts int `json:"excerpts"`
	Lines    int `json:"lines"`
	Bytes    int `json:"bytes"`
}

// EvidenceDigest is the compact dashboard-selected critic evidence contract.
type EvidenceDigest struct {
	SchemaVersion      int                    `json:"schema_version"`
	Hash               string                 `json:"hash"`
	SourceEvidenceHash string                 `json:"source_evidence_hash"`
	BundleHash         string                 `json:"bundle_hash"`
	ProvenanceHash     string                 `json:"provenance_hash"`
	EncodedBytes       int                    `json:"encoded_bytes"`
	SelectedLines      int                    `json:"selected_lines"`
	Lines              []DigestLine           `json:"lines"`
	Provenance         []DigestProvenanceLine `json:"-"`
	Omitted            DigestOmissions        `json:"omitted"`
}

// DigestTelemetry is the bounded private provenance retained with one trial.
type DigestTelemetry struct {
	SchemaVersion       int                    `json:"schema_version"`
	Hash                string                 `json:"hash"`
	SourceEvidenceHash  string                 `json:"source_evidence_hash"`
	BundleHash          string                 `json:"bundle_hash"`
	ProvenanceHash      string                 `json:"provenance_hash"`
	EncodedBytes        int                    `json:"encoded_bytes"`
	SelectedLines       int                    `json:"selected_lines"`
	ProvenanceAvailable bool                   `json:"provenance_available"`
	Provenance          []DigestProvenanceLine `json:"provenance,omitempty"`
	Omitted             DigestOmissions        `json:"omitted"`
}

type digestCandidate struct {
	category  string
	priority  int
	mandatory bool
	source    EvidenceReference
	pathLine  int
	text      string
	truncated bool
}

// BuildEvidenceDigest selects a deterministic compact evidence bundle.
func BuildEvidenceDigest(bundle agentanalysis.EvidenceBundle, draft Draft) (agentanalysis.EvidenceBundle, EvidenceDigest, error) {
	if err := agentanalysis.ValidateEvidenceBundle(bundle); err != nil {
		return agentanalysis.EvidenceBundle{}, EvidenceDigest{}, validationError(ValidationInputEvidence, ErrInvalidInput, "%v", err)
	}
	draft = canonicalDraft(draft)
	if err := validateDraft(draft, bundle); err != nil {
		return agentanalysis.EvidenceBundle{}, EvidenceDigest{}, err
	}
	candidates := digestCandidates(bundle, draft)
	mandatory := make([]digestCandidate, 0, len(candidates))
	optional := make([]digestCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.mandatory {
			mandatory = append(mandatory, candidate)
		} else {
			optional = append(optional, candidate)
		}
	}
	sortDigestCandidates(mandatory)
	sortDigestCandidates(optional)
	selected := slices.Clone(mandatory)
	reduced, digest, err := buildDigestPackage(bundle, selected)
	if err != nil {
		return agentanalysis.EvidenceBundle{}, EvidenceDigest{}, err
	}
	if digest.EncodedBytes > DigestHardLimitBytes {
		return agentanalysis.EvidenceBundle{}, EvidenceDigest{}, validationError(ValidationInputSize, ErrInvalidInput, "mandatory critic digest exceeds %d bytes", DigestHardLimitBytes)
	}
	if digest.EncodedBytes <= DigestTargetBytes {
		for _, candidate := range optional {
			trial := append(slices.Clone(selected), candidate)
			trialBundle, trialDigest, trialErr := buildDigestPackage(bundle, trial)
			if trialErr != nil || trialDigest.EncodedBytes > DigestTargetBytes {
				continue
			}
			selected, reduced, digest = trial, trialBundle, trialDigest
		}
	}
	if digest.EncodedBytes > DigestHardLimitBytes {
		return agentanalysis.EvidenceBundle{}, EvidenceDigest{}, validationError(ValidationInputSize, ErrInvalidInput, "critic digest exceeds %d bytes", DigestHardLimitBytes)
	}
	return reduced, digest, nil
}

// ValidateEvidenceDigest verifies the compact evidence identity and references.
func ValidateEvidenceDigest(digest EvidenceDigest, bundle agentanalysis.EvidenceBundle) error {
	if digest.SchemaVersion != DigestSchemaVersion || !validSHA256(digest.SourceEvidenceHash) || digest.BundleHash != bundle.Hash || !validSHA256(digest.ProvenanceHash) || !validSHA256(digest.Hash) {
		return validationError(ValidationInputIdentity, ErrInvalidInput, "critic digest identity is invalid")
	}
	if err := agentanalysis.ValidateEvidenceBundle(bundle); err != nil {
		return validationError(ValidationInputEvidence, ErrInvalidInput, "%v", err)
	}
	if digest.SelectedLines != len(digest.Lines) || digest.SelectedLines < 1 || digest.Omitted.Excerpts < 0 || digest.Omitted.Lines < 0 || digest.Omitted.Bytes < 0 {
		return validationError(ValidationInputIdentity, ErrInvalidInput, "critic digest accounting is invalid")
	}
	if got, err := digestValueHash(digest); err != nil || got != digest.Hash {
		return validationError(ValidationInputIdentity, ErrInvalidInput, "critic digest hash changed")
	}
	if got, err := digestPackageBytes(bundle, digest); err != nil || got != digest.EncodedBytes || got > DigestHardLimitBytes {
		return validationError(ValidationInputSize, ErrInvalidInput, "critic digest byte accounting is invalid")
	}
	seen := map[string]bool{}
	for index, line := range digest.Lines {
		if !allowedDigestCategory(line.Category) || line.Text == "" || !utf8.ValidString(line.Text) || len(line.Text) > DigestHardLimitBytes {
			return validationError(ValidationInputIdentity, ErrInvalidInput, "critic digest line %d is invalid", index)
		}
		if err := validateReference(line.Reference, bundle); err != nil {
			return validationError(ValidationInputIdentity, ErrInvalidInput, "critic digest line %d: %v", index, err)
		}
		key := referenceKey(line.Reference)
		if seen[key] {
			return validationError(ValidationInputIdentity, ErrInvalidInput, "critic digest reference %d is duplicated", index)
		}
		seen[key] = true
		if !digestLineMatchesBundle(line, bundle) {
			return validationError(ValidationInputIdentity, ErrInvalidInput, "critic digest line %d changed", index)
		}
	}
	if len(digest.Provenance) > 0 {
		if got, err := digestProvenanceHash(digest.Provenance); err != nil || got != digest.ProvenanceHash {
			return validationError(ValidationInputIdentity, ErrInvalidInput, "critic digest provenance hash changed")
		}
		if len(digest.Provenance) != len(digest.Lines) {
			return validationError(ValidationInputIdentity, ErrInvalidInput, "critic digest provenance count changed")
		}
		for index, provenance := range digest.Provenance {
			line := digest.Lines[index]
			if provenance.Category != line.Category || provenance.Reference != line.Reference || provenance.Truncated != line.Truncated || provenance.SourceReference.ExcerptID == "" || provenance.SourceReference.Path == "" || provenance.SourceReference.LineStart < 1 || provenance.SourceReference.LineEnd != provenance.SourceReference.LineStart {
				return validationError(ValidationInputIdentity, ErrInvalidInput, "critic digest provenance %d is invalid", index)
			}
		}
	}
	return nil
}

func digestTelemetry(digest *EvidenceDigest) *DigestTelemetry {
	if digest == nil {
		return nil
	}
	return &DigestTelemetry{
		SchemaVersion: digest.SchemaVersion, Hash: digest.Hash, SourceEvidenceHash: digest.SourceEvidenceHash,
		BundleHash: digest.BundleHash, ProvenanceHash: digest.ProvenanceHash, EncodedBytes: digest.EncodedBytes, SelectedLines: digest.SelectedLines,
		ProvenanceAvailable: true, Provenance: slices.Clone(digest.Provenance), Omitted: digest.Omitted,
	}
}

func digestCandidates(bundle agentanalysis.EvidenceBundle, draft Draft) []digestCandidate {
	byID := make(map[string]agentanalysis.EvidenceExcerpt, len(bundle.Excerpts))
	for _, excerpt := range bundle.Excerpts {
		byID[excerpt.ID] = excerpt
	}
	selected := map[string]digestCandidate{}
	add := func(candidate digestCandidate) {
		key := referenceKey(candidate.source)
		prior, ok := selected[key]
		if !ok || candidate.priority > prior.priority || candidate.mandatory && !prior.mandatory {
			selected[key] = candidate
		}
	}
	for _, citation := range draft.EvidenceCitations {
		excerpt, start, end, ok := locateGroundedCitation(citation, bundle)
		if !ok {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(excerpt.Content, "\r\n", "\n"), "\n")
		for line := max(1, start-2); line <= min(len(lines), end+2); line++ {
			category := DigestCategoryCitationContext
			priority := 90
			mandatory := line >= start-1 && line <= end+1
			candidate := makeDigestCandidate(excerpt, line, lines[line-1], category, priority, mandatory)
			if line >= start && line <= end {
				offset := line - start
				candidate.category, candidate.priority, candidate.mandatory = DigestCategoryCitation, 100, true
				candidate.pathLine = citation.LineStart + offset
				candidate.truncated = false
			}
			add(candidate)
		}
	}
	for _, line := range selectEvidenceLines(bundle, errorLineRE, nil, 8) {
		add(candidateFromEvidenceLine(line, byID, DigestCategorySpecificError, 80))
	}
	for _, line := range selectEvidenceLines(bundle, digestTimelineRE, nil, 8) {
		add(candidateFromEvidenceLine(line, byID, DigestCategoryTimeline, 70))
	}
	for _, line := range selectEvidenceLines(bundle, successLineRE, negativeRE, 6) {
		add(candidateFromEvidenceLine(line, byID, DigestCategorySuccess, 75))
	}
	for _, line := range selectEvidenceLines(bundle, digestOwnershipRE, nil, 6) {
		add(candidateFromEvidenceLine(line, byID, DigestCategoryOwnership, 50))
	}
	if len(selected) == 0 {
		for _, excerpt := range bundle.Excerpts {
			for index, raw := range strings.Split(strings.ReplaceAll(excerpt.Content, "\r\n", "\n"), "\n") {
				if strings.TrimSpace(raw) == "" {
					continue
				}
				add(makeDigestCandidate(excerpt, index+1, raw, DigestCategoryContext, 10, false))
				if len(selected) == 4 {
					break
				}
			}
			if len(selected) == 4 {
				break
			}
		}
	}
	out := make([]digestCandidate, 0, len(selected))
	for _, candidate := range selected {
		out = append(out, candidate)
	}
	return out
}

func candidateFromEvidenceLine(line EvidenceLine, byID map[string]agentanalysis.EvidenceExcerpt, category string, priority int) digestCandidate {
	excerpt := byID[line.Reference.ExcerptID]
	raw := line.Text
	lines := strings.Split(strings.ReplaceAll(excerpt.Content, "\r\n", "\n"), "\n")
	if line.Reference.LineStart >= 1 && line.Reference.LineStart <= len(lines) {
		raw = lines[line.Reference.LineStart-1]
	}
	return makeDigestCandidate(excerpt, line.Reference.LineStart, raw, category, priority, false)
}

func makeDigestCandidate(excerpt agentanalysis.EvidenceExcerpt, line int, raw, category string, priority int, mandatory bool) digestCandidate {
	pathLine, text := digestSourceLine(excerpt, line, raw)
	bounded := text
	truncated := false
	if !mandatory && len(bounded) > maxLineBytes {
		bounded = boundedLine(bounded)
		truncated = true
	}
	return digestCandidate{
		category: category, priority: priority, mandatory: mandatory,
		source:   EvidenceReference{ExcerptID: excerpt.ID, Path: excerpt.Path, LineStart: line, LineEnd: line},
		pathLine: pathLine, text: strings.TrimSpace(bounded), truncated: truncated,
	}
}

func digestSourceLine(excerpt agentanalysis.EvidenceExcerpt, line int, raw string) (int, string) {
	if excerpt.Kind == "grep" {
		if match := digestGrepLineRE.FindStringSubmatch(strings.TrimSpace(raw)); len(match) == 3 {
			if parsed, err := strconv.Atoi(match[1]); err == nil && parsed > 0 {
				return parsed, strings.TrimSpace(match[2])
			}
		}
	}
	return line, strings.TrimSpace(raw)
}

func sortDigestCandidates(values []digestCandidate) {
	sort.Slice(values, func(i, j int) bool {
		if values[i].priority != values[j].priority {
			return values[i].priority > values[j].priority
		}
		if values[i].source.Path != values[j].source.Path {
			return values[i].source.Path < values[j].source.Path
		}
		if values[i].pathLine != values[j].pathLine {
			return values[i].pathLine < values[j].pathLine
		}
		return values[i].source.ExcerptID < values[j].source.ExcerptID
	})
}

func buildDigestPackage(source agentanalysis.EvidenceBundle, selected []digestCandidate) (agentanalysis.EvidenceBundle, EvidenceDigest, error) {
	type pathLine struct {
		candidate digestCandidate
		line      string
	}
	byPath := map[string][]pathLine{}
	seenPathLine := map[string]bool{}
	selectedSource := map[string]bool{}
	selectedBytes := 0
	for _, candidate := range selected {
		if candidate.text == "" {
			continue
		}
		key := fmt.Sprintf("%s\x00%d", candidate.source.Path, candidate.pathLine)
		if seenPathLine[key] {
			continue
		}
		seenPathLine[key] = true
		selectedSource[referenceKey(candidate.source)] = true
		selectedBytes += len(candidate.text)
		byPath[candidate.source.Path] = append(byPath[candidate.source.Path], pathLine{candidate: candidate, line: fmt.Sprintf("> %d: %s", candidate.pathLine, candidate.text)})
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	if len(paths) > 16 {
		return agentanalysis.EvidenceBundle{}, EvidenceDigest{}, fmt.Errorf("critic digest selects more than 16 paths")
	}
	excerpts := make([]agentanalysis.EvidenceExcerpt, 0, len(paths))
	for _, path := range paths {
		lines := byPath[path]
		sort.Slice(lines, func(i, j int) bool {
			if lines[i].candidate.pathLine != lines[j].candidate.pathLine {
				return lines[i].candidate.pathLine < lines[j].candidate.pathLine
			}
			return lines[i].candidate.source.ExcerptID < lines[j].candidate.source.ExcerptID
		})
		content := make([]string, 0, len(lines))
		for _, line := range lines {
			content = append(content, line.line)
		}
		excerpts = append(excerpts, agentanalysis.EvidenceExcerpt{Path: path, Kind: "grep", Content: strings.Join(content, "\n")})
	}
	reduced, err := agentanalysis.NewEvidenceBundle(source.Request, source.Source, source.Scan, nil, excerpts, source.SkillSetHash)
	if err != nil {
		return agentanalysis.EvidenceBundle{}, EvidenceDigest{}, err
	}
	byPathExcerpt := map[string]agentanalysis.EvidenceExcerpt{}
	for _, excerpt := range reduced.Excerpts {
		byPathExcerpt[excerpt.Path] = excerpt
	}
	digestLines := make([]DigestLine, 0, len(selectedSource))
	provenance := make([]DigestProvenanceLine, 0, len(selectedSource))
	for _, path := range paths {
		excerpt := byPathExcerpt[path]
		lines := byPath[path]
		for index, item := range lines {
			reference := EvidenceReference{ExcerptID: excerpt.ID, Path: path, LineStart: index + 1, LineEnd: index + 1}
			digestLines = append(digestLines, DigestLine{Category: item.candidate.category, Reference: reference, Text: item.candidate.text, Truncated: item.candidate.truncated})
			provenance = append(provenance, DigestProvenanceLine{Category: item.candidate.category, SourceReference: item.candidate.source, Reference: reference, Truncated: item.candidate.truncated})
		}
	}
	totalLines, totalBytes, sourceExcerpts := digestSourceTotals(source)
	selectedExcerpts := map[string]bool{}
	for _, line := range provenance {
		selectedExcerpts[line.SourceReference.ExcerptID] = true
	}
	provenanceHash, err := digestProvenanceHash(provenance)
	if err != nil {
		return agentanalysis.EvidenceBundle{}, EvidenceDigest{}, err
	}
	digest := EvidenceDigest{
		SchemaVersion: DigestSchemaVersion, SourceEvidenceHash: source.Hash, BundleHash: reduced.Hash, ProvenanceHash: provenanceHash,
		SelectedLines: len(digestLines), Lines: digestLines, Provenance: provenance,
		Omitted: DigestOmissions{Excerpts: max(sourceExcerpts-len(selectedExcerpts), 0), Lines: max(totalLines-len(selectedSource), 0), Bytes: max(totalBytes-selectedBytes, 0)},
	}
	for range 3 {
		hash, err := digestValueHash(digest)
		if err != nil {
			return agentanalysis.EvidenceBundle{}, EvidenceDigest{}, err
		}
		digest.Hash = hash
		encoded, err := digestPackageBytes(reduced, digest)
		if err != nil {
			return agentanalysis.EvidenceBundle{}, EvidenceDigest{}, err
		}
		if digest.EncodedBytes == encoded {
			break
		}
		digest.EncodedBytes = encoded
	}
	hash, err := digestValueHash(digest)
	if err != nil {
		return agentanalysis.EvidenceBundle{}, EvidenceDigest{}, err
	}
	digest.Hash = hash
	encoded, err := digestPackageBytes(reduced, digest)
	if err != nil {
		return agentanalysis.EvidenceBundle{}, EvidenceDigest{}, err
	}
	digest.EncodedBytes = encoded
	digest.Hash, err = digestValueHash(digest)
	if err != nil {
		return agentanalysis.EvidenceBundle{}, EvidenceDigest{}, err
	}
	return reduced, digest, nil
}

func digestSourceTotals(bundle agentanalysis.EvidenceBundle) (int, int, int) {
	lines, bytes := 0, 0
	for _, excerpt := range bundle.Excerpts {
		for _, raw := range strings.Split(strings.ReplaceAll(excerpt.Content, "\r\n", "\n"), "\n") {
			text := strings.TrimSpace(raw)
			if text == "" {
				continue
			}
			lines++
			bytes += len(text)
		}
	}
	return lines, bytes, len(bundle.Excerpts)
}

func digestProvenanceHash(provenance []DigestProvenanceLine) (string, error) {
	data, err := json.Marshal(provenance)
	if err != nil {
		return "", err
	}
	return hashString(string(data)), nil
}

func digestValueHash(digest EvidenceDigest) (string, error) {
	digest.Hash = ""
	data, err := json.Marshal(digest)
	if err != nil {
		return "", err
	}
	return hashString(string(data)), nil
}

func digestPackageBytes(bundle agentanalysis.EvidenceBundle, digest EvidenceDigest) (int, error) {
	data, err := json.Marshal(struct {
		Bundle agentanalysis.EvidenceBundle `json:"bundle"`
		Digest EvidenceDigest               `json:"digest"`
	}{bundle, digest})
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

func digestLineMatchesBundle(line DigestLine, bundle agentanalysis.EvidenceBundle) bool {
	for _, excerpt := range bundle.Excerpts {
		if excerpt.ID != line.Reference.ExcerptID || excerpt.Path != line.Reference.Path {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(excerpt.Content, "\r\n", "\n"), "\n")
		if line.Reference.LineStart < 1 || line.Reference.LineStart > len(lines) {
			return false
		}
		_, text := digestSourceLine(excerpt, line.Reference.LineStart, lines[line.Reference.LineStart-1])
		return text == line.Text
	}
	return false
}

func allowedDigestCategory(value string) bool {
	switch value {
	case DigestCategoryCitation, DigestCategoryCitationContext, DigestCategorySpecificError, DigestCategoryTimeline, DigestCategorySuccess, DigestCategoryOwnership, DigestCategoryContext:
		return true
	default:
		return false
	}
}
