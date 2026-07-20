package orka

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type fakeTaskExecutionClient struct {
	state          TaskState
	deleted        bool
	remainAfterDel bool
}

func (f *fakeTaskExecutionClient) TaskState(context.Context, string, string) (TaskState, error) {
	if f.deleted && !f.remainAfterDel {
		return TaskState{}, nil
	}
	return f.state, nil
}

func (f *fakeTaskExecutionClient) Delete(context.Context, schema.GroupVersionResource, string, string) error {
	f.deleted = true
	return nil
}

func TestPrepareTaskExecution(t *testing.T) {
	cpu := map[string]any{"nodeSelector": map[string]any{"agentpool": "cpu"}}
	gpu := map[string]any{"nodeSelector": map[string]any{"agentpool": "gpu"}}
	for _, tc := range []struct {
		name       string
		state      TaskState
		desired    map[string]any
		wantSkip   bool
		wantDelete bool
	}{
		{name: "missing", state: TaskState{}, desired: cpu},
		{name: "same failed", state: TaskState{Exists: true, Phase: "Failed", Execution: cpu}, desired: cpu},
		{name: "changed succeeded", state: TaskState{Exists: true, Phase: "Succeeded", Execution: gpu}, desired: cpu, wantSkip: true},
		{name: "changed failed", state: TaskState{Exists: true, Phase: "Failed", Execution: gpu}, desired: cpu, wantDelete: true},
		{name: "changed running", state: TaskState{Exists: true, Phase: "Running", Execution: gpu}, desired: cpu, wantDelete: true},
		{name: "placement removed", state: TaskState{Exists: true, Phase: "Pending", Execution: gpu}, wantDelete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeTaskExecutionClient{state: tc.state}
			skip, err := PrepareTaskExecution(context.Background(), client, "orka-system", "task", tc.desired, time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			if skip != tc.wantSkip || client.deleted != tc.wantDelete {
				t.Fatalf("skip=%v deleted=%v, want skip=%v deleted=%v", skip, client.deleted, tc.wantSkip, tc.wantDelete)
			}
		})
	}
}

func TestPrepareTaskExecutionDeletionTimeout(t *testing.T) {
	client := &fakeTaskExecutionClient{
		state:          TaskState{Exists: true, Phase: "Running", Execution: map[string]any{"nodeSelector": map[string]any{"pool": "old"}}},
		remainAfterDel: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := PrepareTaskExecution(ctx, client, "orka-system", "task", map[string]any{"nodeSelector": map[string]any{"pool": "new"}}, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("error = %v, want deletion timeout", err)
	}
}

func TestTaskExecutionEqualNormalizesJSONNumbers(t *testing.T) {
	a := map[string]any{"tolerations": []any{map[string]any{"tolerationSeconds": int64(300)}}}
	b := map[string]any{"tolerations": []any{map[string]any{"tolerationSeconds": float64(300)}}}
	if !taskExecutionEqual(a, b) {
		t.Fatalf("executions should compare equal: %#v %#v", a, b)
	}
}

func TestTaskStateFromObject(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec":   map[string]any{"execution": map[string]any{"nodeSelector": map[string]any{"agentpool": "cpu"}}},
		"status": map[string]any{"phase": "Running"},
	}}
	state, err := taskStateFromObject(u)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Exists || state.Phase != "Running" || state.Execution["nodeSelector"].(map[string]any)["agentpool"] != "cpu" {
		t.Fatalf("state = %+v", state)
	}
}
