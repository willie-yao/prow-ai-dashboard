package capi

import (
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// Compile-time assertion that the capi module satisfies the optional
// AgenticPreferrer interface. Drop this if PrefersAgentic ever becomes
// expensive enough to want explicit opt-out semantics instead.
var _ ai.AgenticPreferrer = (*Module)(nil)

func TestPrefersAgentic(t *testing.T) {
	m := New("capz-e2e", project.EffectiveEvidence{})

	tests := []struct {
		name     string
		tc       *models.TestCase
		wantPref bool
		wantSub  string
	}{
		{
			name:     "nil test case",
			tc:       nil,
			wantPref: false,
		},
		{
			name:     "nil cluster artifacts",
			tc:       &models.TestCase{Name: "TestX"},
			wantPref: true,
			wantSub:  "no cluster artifacts collected",
		},
		{
			name: "empty machines and pod logs",
			tc: &models.TestCase{
				Name:             "TestY",
				ClusterArtifacts: &models.ClusterArtifacts{ClusterName: "capz-e2e-abc"},
			},
			wantPref: true,
			wantSub:  `cluster "capz-e2e-abc"`,
		},
		{
			name: "has machine logs",
			tc: &models.TestCase{
				ClusterArtifacts: &models.ClusterArtifacts{
					ClusterName: "capz-e2e-abc",
					Machines:    []models.MachineArtifacts{{Name: "cp-0"}},
				},
			},
			wantPref: false,
		},
		{
			name: "has only pod log dirs",
			tc: &models.TestCase{
				ClusterArtifacts: &models.ClusterArtifacts{
					ClusterName: "capz-e2e-abc",
					PodLogDirs:  map[string]string{"capi-system": "https://..."},
				},
			},
			wantPref: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := m.PrefersAgentic(nil, tt.tc)
			if got != tt.wantPref {
				t.Fatalf("PrefersAgentic = %v, want %v (reason=%q)", got, tt.wantPref, reason)
			}
			if tt.wantSub != "" && !contains(reason, tt.wantSub) {
				t.Fatalf("reason %q does not contain %q", reason, tt.wantSub)
			}
			if !tt.wantPref && reason != "" {
				t.Fatalf("non-prefer case returned non-empty reason %q", reason)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
