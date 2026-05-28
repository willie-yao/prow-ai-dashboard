package capi

import (
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
)

// TestNewAcceptsEmptyPrefix verifies that the collector constructs
// successfully without a cluster_name_prefix, which is the CAPI core
// configuration (one dir per cluster, cluster name == namespace).
func TestNewAcceptsEmptyPrefix(t *testing.T) {
	c, err := New(gcs.NewBucket("kubernetes-ci-logs"), nil, "")
	if err != nil {
		t.Fatalf("New(empty prefix) errored: %v", err)
	}
	if c.nsPrefixRe != nil {
		t.Errorf("expected nil nsPrefixRe for empty prefix, got %v", c.nsPrefixRe)
	}
}

func TestBootstrapNamespace(t *testing.T) {
	cases := []struct {
		name        string
		prefix      string
		clusterName string
		want        string
	}{
		{"capz-style with prefix extracts namespace", "capz-e2e", "capz-e2e-abc123-azl3", "capz-e2e-abc123"},
		{"capz-style no namespace match returns empty", "capz-e2e", "unrelated-cluster", ""},
		{"capi-core empty prefix uses full cluster name", "", "quick-start-bxqxxs", "quick-start-bxqxxs"},
		{"empty cluster name returns empty", "", "", ""},
		{"empty cluster name with prefix returns empty", "capz-e2e", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(gcs.NewBucket("kubernetes-ci-logs"), nil, tc.prefix)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if got := c.bootstrapNamespace(tc.clusterName); got != tc.want {
				t.Errorf("bootstrapNamespace(%q) = %q, want %q", tc.clusterName, got, tc.want)
			}
		})
	}
}
