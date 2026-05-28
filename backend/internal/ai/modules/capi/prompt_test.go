package capi

import (
	"strings"
	"testing"
)

// TestBuildAnalysisPromptByteIdentity pins the structural anchors of the
// combined summary + root-cause prompt. The body is kept compatible with the
// pre-unification "comprehensive:<hash>" cache shape. The response JSON
// schema now lives in the engine's shared ResponseFormatFooter, so it is no
// longer asserted on the user prompt. Bump this test deliberately if the
// prompt changes.
func TestBuildAnalysisPromptByteIdentity(t *testing.T) {
	ev := evidence{
		TestName:         "TestControlPlane",
		FailureMessage:   "Timed out",
		FailureBody:      "stack trace here",
		ClusterFlavor:    "prow-azl3",
		ConsecutiveCount: 3,
		BuildLogErrors:   "FATAL: kubeadm init failed",
	}
	got := buildAnalysisPrompt(ev)

	// Spot-check stable structural anchors rather than the whole blob.
	anchors := []string{
		"Investigate this CAPI E2E test failure using the artifact data below.",
		"Test: TestControlPlane\n",
		"Flavor: prow-azl3\n",
		"Failed 3 consecutive times\n",
		"Error: Timed out\n",
		"Stack trace:\nstack trace here\n",
		"=== Build Log Errors ===\nFATAL: kubeadm init failed\n",
		"1. ROOT CAUSE:",
		"2. TRACE THE FAILURE:",
		"4. SUMMARY:",
	}
	for _, a := range anchors {
		if !strings.Contains(got, a) {
			t.Errorf("analysis prompt missing anchor %q\nfull prompt:\n%s", a, got)
		}
	}

	// The engine prompt must NOT carry project-specific failure-chain
	// vocabulary. That knowledge belongs in the consumer's system.md.
	forbidden := []string{
		"VM provisioning",
		"cloud-init → kubeadm",
		"CCM → providerID",
		"CAPI dependency chain",
	}
	for _, f := range forbidden {
		if strings.Contains(got, f) {
			t.Errorf("analysis prompt leaked project-specific phrase %q\nfull prompt:\n%s", f, got)
		}
	}
}
