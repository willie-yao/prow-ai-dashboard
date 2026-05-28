package capi

import (
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

func TestIsKnownTransient(t *testing.T) {
	ev, err := (&project.Config{}).EffectiveEvidence()
	if err != nil {
		t.Fatalf("EffectiveEvidence: %v", err)
	}
	m := New("capz-e2e", ev)
	cases := []struct {
		msg  string
		want string
	}{
		{"HTTP 429 Too Many Requests", "Cloud API throttling (HTTP 429)"},
		{"Azure throttling on resource group", "Cloud API throttling (HTTP 429)"},
		{"Too many requests from client", "Cloud API throttling (HTTP 429)"},
		{"quota exceeded for StandardDSv3Family", "Cloud resource quota exceeded"},
		{"resource quota limit reached", "Cloud resource quota exceeded"},
		{"context deadline exceeded during cleanup", "Context deadline during cleanup"},
		{"context deadline exceeded: delete resource group", "Context deadline during cleanup"},
		{"dns resolution failed for mcr.microsoft.com", "DNS resolution failure"},
		{"dns lookup failed for storage.googleapis.com", "DNS resolution failure"},
		{"ImagePullBackOff for calico-node", "Image pull backoff (transient)"},
		{"no space left on device", "Disk space exhausted"},
		{"kubelet certificate expired", ""},
		{"control plane never initialized", ""},
		{"calico-node CrashLoopBackOff", ""},
	}
	for _, tc := range cases {
		got := m.IsKnownTransient(tc.msg)
		if got != tc.want {
			t.Errorf("IsKnownTransient(%q) = %q, want %q", tc.msg, got, tc.want)
		}
	}
}
