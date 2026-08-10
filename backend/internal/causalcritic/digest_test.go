package causalcritic

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

func TestNewDigestInputIsDeterministicAndBounded(t *testing.T) {
	bundle := digestTestBundle(t, false)
	authoritative := digestTestAuthoritative()
	first, err := NewDigestInput(bundle, authoritative)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateInput(first); err != nil {
		t.Fatal(err)
	}
	secondBundle := digestTestBundle(t, true)
	second, err := NewDigestInput(secondBundle, authoritative)
	if err != nil {
		t.Fatal(err)
	}
	if first.PairHash != second.PairHash || first.Bundle.Hash != second.Bundle.Hash || first.Digest == nil || second.Digest == nil || first.Digest.Hash != second.Digest.Hash {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	if first.Digest.SourceEvidenceHash != bundle.Hash || first.Digest.BundleHash != first.Bundle.Hash || first.Digest.EncodedBytes > DigestHardLimitBytes || first.Digest.SelectedLines != len(first.Digest.Lines) {
		t.Fatalf("digest=%+v", first.Digest)
	}
	if first.Digest.EncodedBytes > DigestTargetBytes {
		t.Logf("mandatory citation context exceeded target: %d bytes", first.Digest.EncodedBytes)
	}
}

func TestDigestPreservesExactCitationPathWithRepeatedText(t *testing.T) {
	input, err := NewDigestInput(digestTestBundle(t, false), digestTestAuthoritative())
	if err != nil {
		t.Fatal(err)
	}
	foundCitation := false
	for index, line := range input.Digest.Lines {
		if line.Category != DigestCategoryCitation {
			continue
		}
		foundCitation = true
		if input.Digest.Provenance[index].SourceReference.Path != "logs/a.log" || line.Reference.Path != "logs/a.log" {
			t.Fatalf("citation line=%+v provenance=%+v", line, input.Digest.Provenance[index])
		}
	}
	if !foundCitation {
		t.Fatal("digest omitted authoritative citation")
	}
	if len(input.CitedEvidence) == 0 || input.CitedEvidence[0].Reference.Path != "logs/a.log" {
		t.Fatalf("cited evidence=%+v", input.CitedEvidence)
	}
}

func TestDigestPreservesCitationThroughNestedGrepPrefixes(t *testing.T) {
	base := digestTestBundle(t, false)
	bundle, err := agentanalysis.NewEvidenceBundle(
		base.Request, base.Source, base.Scan, nil,
		[]agentanalysis.EvidenceExcerpt{{
			Path: "node-describe.txt", Kind: "grep",
			Content: "> 15: CreationTimestamp:  Sat, 04 Jul 2026 04:38:41 +0000\n  10: 16: Taints:             node.cloudprovider.kubernetes.io/uninitialized=true:NoSchedule\n",
		}},
		base.SkillSetHash,
	)
	if err != nil {
		t.Fatal(err)
	}
	authoritative := digestTestAuthoritative()
	authoritative.EvidenceCitations = []models.EvidenceCitation{{
		Path: "node-describe.txt", LineStart: 15, LineEnd: 16,
		Quote: "CreationTimestamp:  Sat, 04 Jul 2026 04:38:41 +0000\nTaints:             node.cloudprovider.kubernetes.io/uninitialized",
	}}
	input, err := NewDigestInput(bundle, authoritative)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Bundle.Excerpts) != 1 {
		t.Fatalf("excerpts=%+v", input.Bundle.Excerpts)
	}
	content := input.Bundle.Excerpts[0].Content
	if !strings.Contains(content, "> 15: CreationTimestamp:  Sat, 04 Jul 2026 04:38:41 +0000\n> 16: 16: Taints:             node.cloudprovider.kubernetes.io/uninitialized") {
		t.Fatalf("digest citation was not anchored to authoritative lines: %q", content)
	}
}

func TestDigestIncludesGenericCausalCategories(t *testing.T) {
	input, err := NewDigestInput(digestTestBundle(t, false), digestTestAuthoritative())
	if err != nil {
		t.Fatal(err)
	}
	categories := map[string]bool{}
	for _, line := range input.Digest.Lines {
		categories[line.Category] = true
	}
	for _, want := range []string{DigestCategoryCitation, DigestCategoryCitationContext, DigestCategorySpecificError, DigestCategoryTimeline, DigestCategorySuccess, DigestCategoryOwnership} {
		if !categories[want] {
			t.Fatalf("digest categories=%v, missing %s", categories, want)
		}
	}
}

func TestValidateEvidenceDigestRejectsTampering(t *testing.T) {
	input, err := NewDigestInput(digestTestBundle(t, false), digestTestAuthoritative())
	if err != nil {
		t.Fatal(err)
	}
	tampered := input
	digest := *input.Digest
	digest.Lines = slices.Clone(input.Digest.Lines)
	digest.Lines[0].Text = "changed"
	tampered.Digest = &digest
	if err := ValidateInput(tampered); ValidationCodeOf(err) != ValidationInputIdentity {
		t.Fatalf("err=%v", err)
	}
}

func digestTestBundle(t *testing.T, reverse bool) agentanalysis.EvidenceBundle {
	t.Helper()
	excerpts := []agentanalysis.EvidenceExcerpt{
		{Path: "logs/a.log", Kind: "tail", Content: "controller started reconciliation\nGET widgets.example.io/v2 returned 404 unsupported while v1 was served\ndeployment timed out waiting for readiness\n"},
		{Path: "logs/b.log", Kind: "tail", Content: "GET widgets.example.io/v2 returned 404 unsupported while v1 was served\n2026-08-10T09:59:00Z reconciliation started\n2026-08-10T10:00:00Z retry scheduled after API error\n"},
		{Path: "logs/c.log", Kind: "tail", Content: "the operator owns deployment/example\nlater the same controller reconciled and became Ready\n"},
	}
	if reverse {
		slices.Reverse(excerpts)
	}
	bundle, err := agentanalysis.NewEvidenceBundle(
		ai.FailureAnalysisRequest{
			JobID: "periodic::job", BuildPrefix: "logs/job/1/",
			Build:    models.BuildInfo{BuildID: "1", JobName: "job", RepoRefs: map[string]string{"example/repo": strings.Repeat("a", 40)}},
			TestCase: models.TestCase{Name: "TestFailure", Status: "failed", FailureMessage: "deployment timed out"},
		},
		sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)},
		agentanalysis.ArtifactScan{PathCount: len(excerpts)}, nil, excerpts, strings.Repeat("b", 64),
	)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func digestTestAuthoritative() agentanalysis.AuthoritativeSnapshot {
	return agentanalysis.AuthoritativeSnapshot{
		Summary: "The deployment timed out.", RootCause: "The readiness timeout caused the failure.", Severity: "High", SuggestedFix: "Investigate the earlier API error.",
		EvidenceCitations: []models.EvidenceCitation{{Path: "logs/a.log", LineStart: 2, LineEnd: 2, Quote: "GET widgets.example.io/v2 returned 404 unsupported while v1 was served"}},
	}
}

func TestDigestFallsBackToBoundedContext(t *testing.T) {
	base := digestTestBundle(t, false)
	bundle, err := agentanalysis.NewEvidenceBundle(
		base.Request, base.Source, base.Scan, nil,
		[]agentanalysis.EvidenceExcerpt{{Path: "plain.log", Kind: "tail", Content: "plain unclassified evidence\nanother neutral line\n"}},
		base.SkillSetHash,
	)
	if err != nil {
		t.Fatal(err)
	}
	authoritative := digestTestAuthoritative()
	authoritative.EvidenceCitations = nil
	input, err := NewDigestInput(bundle, authoritative)
	if err != nil {
		t.Fatal(err)
	}
	if input.Digest == nil || len(input.Digest.Lines) == 0 || input.Digest.Lines[0].Category != DigestCategoryContext {
		t.Fatalf("digest=%+v", input.Digest)
	}
}

func TestDigestRejectsMandatoryEvidenceBeyondHardCap(t *testing.T) {
	base := digestTestBundle(t, false)
	line := "required citation " + strings.Repeat("x", DigestHardLimitBytes)
	bundle, err := agentanalysis.NewEvidenceBundle(
		base.Request, base.Source, base.Scan, nil,
		[]agentanalysis.EvidenceExcerpt{{Path: "large.log", Kind: "tail", Content: line}},
		base.SkillSetHash,
	)
	if err != nil {
		t.Fatal(err)
	}
	authoritative := digestTestAuthoritative()
	authoritative.EvidenceCitations = []models.EvidenceCitation{{Path: "large.log", LineStart: 1, LineEnd: 1, Quote: "required citation"}}
	if _, err := NewDigestInput(bundle, authoritative); ValidationCodeOf(err) != ValidationInputSize {
		t.Fatalf("err=%v", err)
	}
}

func TestDigestModelContractExposesOnlyValidReferences(t *testing.T) {
	input, err := NewDigestInput(digestTestBundle(t, false), digestTestAuthoritative())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "provenance_hash") {
		t.Fatalf("model input omitted provenance hash: %s", data)
	}
	if strings.Contains(string(data), "source_reference") {
		t.Fatalf("model input exposed private source provenance: %s", data)
	}
	var decoded Input
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := ValidateInput(decoded); err != nil {
		t.Fatal(err)
	}
	reference := decoded.Digest.Lines[0].Reference
	review := Review{
		SchemaVersion: ReviewSchemaVersion, ContractVersion: ContractVersion, PairHash: decoded.PairHash, Verdict: "object", Confidence: "medium",
		Findings:         []Finding{{Class: FindingCausalLinkUnsupported, Detail: "The draft does not establish the earlier event.", References: []EvidenceReference{reference}}},
		RevisionGuidance: "Explain the earlier supported event.",
	}
	if err := ValidateReview(review, decoded); err != nil {
		t.Fatal(err)
	}
}

func TestValidateEvidenceDigestRejectsPrivateProvenanceTampering(t *testing.T) {
	input, err := NewDigestInput(digestTestBundle(t, false), digestTestAuthoritative())
	if err != nil {
		t.Fatal(err)
	}
	tampered := input
	digest := *input.Digest
	digest.Lines = slices.Clone(input.Digest.Lines)
	digest.Provenance = slices.Clone(input.Digest.Provenance)
	digest.Provenance[0].SourceReference.Path = "unrelated.log"
	tampered.Digest = &digest
	if err := ValidateInput(tampered); ValidationCodeOf(err) != ValidationInputIdentity {
		t.Fatalf("err=%v", err)
	}
}
