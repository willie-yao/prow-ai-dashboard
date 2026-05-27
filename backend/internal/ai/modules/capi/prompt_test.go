package capi

import (
	"strings"
	"testing"
)

// TestQuickSummaryPromptByteIdentity pins the exact byte content of the quick
// prompt. The text is loaded by CAPZ today; changing it would invalidate every
// cached "summary:<hash>" entry. Bump this test deliberately if the prompt
// changes.
func TestQuickSummaryPromptByteIdentity(t *testing.T) {
	m := New("capz-e2e")
	got := m.QuickSummaryPrompt(
		"TestControlPlane",
		"HTTP 429 Too Many Requests",
		"machine_test.go:42",
	)
	want := "Give a brief 1-2 sentence summary of why this CAPZ E2E test failed.\n\n" +
		"Test: TestControlPlane\n" +
		"Error: HTTP 429 Too Many Requests\n" +
		"Location: machine_test.go:42\n\n" +
		"Respond in JSON: {\"summary\": \"...\", \"is_transient\": true/false}"
	if got != want {
		t.Errorf("QuickSummaryPrompt mismatch:\n--- got ---\n%s\n--- want ---\n%s\n", got, want)
	}
}

// TestBuildDeepPromptByteIdentity pins the deep-analysis prompt format. Any
// drift here invalidates "comprehensive:<hash>" cache entries.
func TestBuildDeepPromptByteIdentity(t *testing.T) {
	ev := evidence{
		TestName:         "TestControlPlane",
		FailureMessage:   "Timed out",
		FailureBody:      "stack trace here",
		ClusterFlavor:    "prow-azl3",
		ConsecutiveCount: 3,
		BuildLogErrors:   "FATAL: kubeadm init failed",
	}
	got := buildDeepPrompt(ev)

	// Spot-check stable structural anchors rather than the whole blob.
	anchors := []string{
		"Investigate this CAPZ E2E test failure using the artifact data below.",
		"Test: TestControlPlane\n",
		"Flavor: prow-azl3\n",
		"Failed 3 consecutive times\n",
		"Error: Timed out\n",
		"Stack trace:\nstack trace here\n",
		"=== Build Log Errors ===\nFATAL: kubeadm init failed\n",
		"1. ROOT CAUSE:",
		`Respond in JSON: {"root_cause":`,
	}
	for _, a := range anchors {
		if !strings.Contains(got, a) {
			t.Errorf("deep prompt missing anchor %q\nfull prompt:\n%s", a, got)
		}
	}
}
