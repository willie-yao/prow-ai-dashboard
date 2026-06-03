package ai

import (
	"strings"
	"testing"
)

// TestPuntRE_SanityTable mirrors the 12-case sanity check used to
// validate the Python A/B harness regex in build_ab_l4s1.py. Keeping
// the table identical means a future regex tweak that breaks Python /
// Go agreement will surface in CI.
func TestPuntRE_SanityTable(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		wantPunt bool
	}{
		{
			name: "KCP HA punt example (pre-L.4)",
			text: "Investigate Azure VM provisioning failures for the third control plane node. Check AzureMachine resource status conditions and ensure proper DNS configuration. Verify Azure quotas and network security rules allowing SSH access.",
			wantPunt: true,
		},
		{
			name:     "Clean Claude-style fix",
			text:     "Update kustomize/cluster-template-prow-azl3-flatcar-private.yaml line 142 to set virtualNetwork.vnetPeerings to match the staging vnet name. Reapply and retry the conformance job.",
			wantPunt: false,
		},
		{
			name:     "Composite verify-by",
			text:     "Bump the kube-vip image to v0.7.2 in templates/addons/kube-vip.yaml, then verify by tailing the kube-vip pod logs for the new image tag.",
			wantPunt: false,
		},
		{
			name:     "Apply then verify-by",
			text:     "Apply the fix; verify by rerunning the e2e suite.",
			wantPunt: false,
		},
		{
			name:     "Should-check punt",
			text:     "You should check whether the controller manager log shows leader election failures.",
			wantPunt: true,
		},
		{
			name:     "No-remediation escape hatch",
			text:     "No remediation possible from available evidence: artifacts show the kubelet failed to register but the journal.log was truncated before the error; would need a fresh build with the journal preserved.",
			wantPunt: false,
		},
		{
			name:     "Recommend punt mid-sentence",
			text:     "We recommend checking the AzureMachine status conditions and reviewing the cloud-init logs.",
			wantPunt: true,
		},
		{
			name:     "Operator should investigate",
			text:     "The operator should investigate why the VMSS failed to scale up.",
			wantPunt: true,
		},
		{
			name:     "Recommend at start",
			text:     "Recommend reviewing the prow-azl3 template diff against main.",
			wantPunt: true,
		},
		{
			name:     "Confirm via dashboard",
			text:     "Patch templates/addons/kube-vip.yaml; confirm via the e2e dashboard that the test passes.",
			wantPunt: false,
		},
		{
			name:     "You should verify by",
			text:     "You should verify by checking the metrics dashboard.",
			wantPunt: false,
		},
		{
			name:     "Mixed investigate-and-fix still punts",
			text:     "Investigate why the AzureMachine controller is logging x509 errors, and update the secret-name in the webhook config to match the new cert.",
			wantPunt: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := critiqueDraft(analysisResponse{SuggestedFix: tc.text})
			gotPunt := !out.Passed
			if gotPunt != tc.wantPunt {
				t.Errorf("punt=%v, want %v\nmatches=%v\ntext=%q",
					gotPunt, tc.wantPunt, out.Matches, tc.text)
			}
		})
	}
}

// TestCritiqueDraft_PassedReturnsEmptyFeedback verifies the
// passed-path contract: caller can rely on Feedback being empty so
// the agentic loop doesn't accidentally append a no-op user message.
func TestCritiqueDraft_PassedReturnsEmptyFeedback(t *testing.T) {
	out := critiqueDraft(analysisResponse{
		SuggestedFix: "Update kustomize/cluster-template.yaml line 42 to bump the kube-vip image tag.",
	})
	if !out.Passed {
		t.Fatalf("expected passed, got %+v", out)
	}
	if out.Feedback != "" {
		t.Errorf("Feedback should be empty when Passed=true, got %q", out.Feedback)
	}
	if len(out.Matches) != 0 {
		t.Errorf("Matches should be empty when Passed=true, got %v", out.Matches)
	}
}

// TestCritiqueDraft_FeedbackQuotesOffendingText verifies that on
// failure the feedback message quotes the model's own suggested_fix
// and lists the matched phrases. The model needs to see exactly what
// tripped the gate so it can re-emit something different — a vague
// "you punted, try again" feedback would just reproduce the same
// punt.
func TestCritiqueDraft_FeedbackQuotesOffendingText(t *testing.T) {
	bad := "Check the AzureMachine status. Verify cloud-init."
	out := critiqueDraft(analysisResponse{SuggestedFix: bad})
	if out.Passed {
		t.Fatalf("expected punt, got passed")
	}
	if !strings.Contains(out.Feedback, bad) {
		t.Errorf("Feedback should quote the offending suggested_fix\nfeedback:\n%s", out.Feedback)
	}
	for _, m := range out.Matches {
		if !strings.Contains(out.Feedback, m) {
			t.Errorf("Feedback should list matched phrase %q\nfeedback:\n%s", m, out.Feedback)
		}
	}
	// Re-state the two allowed shapes so the retry has a clear target.
	for _, anchor := range []string{
		"CONCRETE remediation",
		"No remediation possible from available evidence",
		"Do NOT re-emit the same draft",
	} {
		if !strings.Contains(out.Feedback, anchor) {
			t.Errorf("Feedback missing anchor %q\nfeedback:\n%s", anchor, out.Feedback)
		}
	}
}

// TestCritiqueDraft_FeedbackDeduplicatesMatches: a punt that hits the
// regex 5x on the same phrase (e.g. "check ... check ... check")
// should only quote that phrase once in the feedback message to keep
// the user-message short.
func TestCritiqueDraft_FeedbackDeduplicatesMatches(t *testing.T) {
	repeat := "Check A. Check B. Check C. Check D."
	out := critiqueDraft(analysisResponse{SuggestedFix: repeat})
	if out.Passed {
		t.Fatalf("expected punt")
	}
	// Conservatively: feedback should contain "Check" but not list it
	// four separate times in the "matched: ..." block.
	matchedSection := out.Feedback[strings.Index(out.Feedback, "(matched:"):]
	if strings.Count(strings.ToLower(matchedSection), `"check"`) > 1 {
		t.Errorf("Feedback should list 'Check' once, got:\n%s", matchedSection)
	}
}

// TestCritiqueDraft_EmptySuggestedFixPasses: edge case. An empty
// suggested_fix can't punt by definition (nothing to match). The
// upstream caller is responsible for treating empty fixes as a
// separate quality signal; critique just checks for punt patterns.
func TestCritiqueDraft_EmptySuggestedFixPasses(t *testing.T) {
	out := critiqueDraft(analysisResponse{SuggestedFix: ""})
	if !out.Passed {
		t.Errorf("empty suggested_fix should pass critique, got %+v", out)
	}
}
