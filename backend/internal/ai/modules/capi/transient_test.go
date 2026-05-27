package capi

import "testing"

func TestIsKnownTransient(t *testing.T) {
	m := New("capz-e2e")
	cases := []struct {
		msg  string
		want string
	}{
		{"HTTP 429 Too Many Requests", "Azure API throttling (HTTP 429)"},
		{"Azure throttling on resource group", "Azure API throttling (HTTP 429)"},
		{"Too many requests from client", "Azure API throttling (HTTP 429)"},
		{"quota exceeded for StandardDSv3Family", "Azure resource quota exceeded"},
		{"resource quota limit reached", "Azure resource quota exceeded"},
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
