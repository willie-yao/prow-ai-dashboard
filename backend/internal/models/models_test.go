package models

import (
	"encoding/json"
	"testing"
)

// Cached job JSON written before the AzureActivityLog→ProviderActivityLog
// rename uses the legacy key. UnmarshalJSON copies it forward so the
// Provider Activity Log link survives the cache rollover window.
func TestClusterArtifacts_LegacyAzureActivityLog(t *testing.T) {
	raw := []byte(`{
		"cluster_name": "capz-e2e-abc",
		"azure_activity_log": "https://gcsweb.example/azure-activity-logs/capz-e2e-abc.log"
	}`)
	var got ClusterArtifacts
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ProviderActivityLog == "" {
		t.Fatalf("expected legacy azure_activity_log to migrate into ProviderActivityLog, got empty")
	}
	if got.ClusterName != "capz-e2e-abc" {
		t.Errorf("ClusterName = %q", got.ClusterName)
	}
}

func TestClusterArtifacts_NewKeyTakesPrecedence(t *testing.T) {
	raw := []byte(`{
		"cluster_name": "x",
		"provider_activity_log": "new-link",
		"azure_activity_log": "legacy-link"
	}`)
	var got ClusterArtifacts
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ProviderActivityLog != "new-link" {
		t.Errorf("ProviderActivityLog = %q, want %q", got.ProviderActivityLog, "new-link")
	}
}

func TestClusterArtifacts_RoundTrip(t *testing.T) {
	in := ClusterArtifacts{
		ClusterName:         "x",
		ProviderActivityLog: "link",
		Machines: []MachineArtifacts{
			{Name: "m1", Logs: map[string]string{"kubelet": "u"}},
		},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out ClusterArtifacts
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ProviderActivityLog != "link" || out.ClusterName != "x" || len(out.Machines) != 1 {
		t.Errorf("round-trip mismatch: %+v", out)
	}
}
