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

// TestBuildAnalysisPrompt_MachineAndControllerLogs verifies that map-based
// evidence is rendered as one section per declared filename / namespace, in
// sorted order so the prompt is deterministic across runs.
func TestBuildAnalysisPrompt_MachineAndControllerLogs(t *testing.T) {
	ev := evidence{
		TestName: "TestExample",
		MachineLogs: map[string]string{
			"kubelet.log":    "kubelet log content",
			"containerd.log": "containerd log content",
		},
		ControllerLogs: map[string]string{
			"capi-system":                       "capi-controller log content",
			"capi-kubeadm-control-plane-system": "kcp log content",
		},
	}
	got := buildAnalysisPrompt(ev)

	for _, anchor := range []string{
		"=== Machine log: containerd.log ===\ncontainerd log content\n",
		"=== Machine log: kubelet.log ===\nkubelet log content\n",
		"=== Controller log: capi-kubeadm-control-plane-system ===\nkcp log content\n",
		"=== Controller log: capi-system ===\ncapi-controller log content\n",
	} {
		if !strings.Contains(got, anchor) {
			t.Errorf("missing anchor %q\nfull prompt:\n%s", anchor, got)
		}
	}

	// Sections must be in sorted-key order.
	containerdIdx := strings.Index(got, "Machine log: containerd.log")
	kubeletIdx := strings.Index(got, "Machine log: kubelet.log")
	if containerdIdx < 0 || kubeletIdx < 0 || containerdIdx > kubeletIdx {
		t.Errorf("machine logs not in sorted order: containerd at %d, kubelet at %d", containerdIdx, kubeletIdx)
	}
	kcpIdx := strings.Index(got, "Controller log: capi-kubeadm-control-plane-system")
	capiIdx := strings.Index(got, "Controller log: capi-system")
	if kcpIdx < 0 || capiIdx < 0 || kcpIdx > capiIdx {
		t.Errorf("controller logs not in sorted order: kcp at %d, capi-system at %d", kcpIdx, capiIdx)
	}
}

// TestBuildAnalysisPrompt_MissingFooter verifies the "Configured but missing"
// footer surfaces requested-but-absent evidence so the AI doesn't infer a
// root cause from absence.
func TestBuildAnalysisPrompt_MissingFooter(t *testing.T) {
	ev := evidence{
		TestName:            "TestExample",
		RequestedButMissing: []string{"machine log boot.log", "controller log capz-system"},
	}
	got := buildAnalysisPrompt(ev)

	for _, anchor := range []string{
		"=== Configured but missing ===",
		"- machine log boot.log",
		"- controller log capz-system",
		"Do not infer a root cause from their absence",
	} {
		if !strings.Contains(got, anchor) {
			t.Errorf("missing anchor %q\nfull prompt:\n%s", anchor, got)
		}
	}
}

// TestBuildAnalysisPrompt_DroppedAllArtifactsWording verifies the prompt no
// longer claims "ALL available artifacts" — that wording is wrong now that
// the evidence set is consumer-configurable. The replacement language
// acknowledges configuration without overpromising.
func TestBuildAnalysisPrompt_DroppedAllArtifactsWording(t *testing.T) {
	got := buildAnalysisPrompt(evidence{TestName: "T"})
	if strings.Contains(got, "ALL available artifacts") {
		t.Errorf("prompt still claims 'ALL available artifacts'\nfull prompt:\n%s", got)
	}
	if !strings.Contains(got, "configured evidence sources") {
		t.Errorf("prompt missing 'configured evidence sources' wording\nfull prompt:\n%s", got)
	}
}
