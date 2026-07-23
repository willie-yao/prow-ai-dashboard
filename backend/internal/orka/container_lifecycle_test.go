package orka

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type fakeContainerResourceClient struct {
	applyErrAt int
	deleteErr  error
	listErr    error
	taskErr    error
	applied    []string
	deleted    []string
	listed     []unstructured.Unstructured
	taskStates map[string]TaskState
}

func (f *fakeContainerResourceClient) Apply(_ context.Context, gvr schema.GroupVersionResource, _ string, obj map[string]any) error {
	name := (&unstructured.Unstructured{Object: obj}).GetName()
	f.applied = append(f.applied, gvr.Resource+"/"+name)
	if f.applyErrAt > 0 && len(f.applied) == f.applyErrAt {
		return errors.New("apply failed")
	}
	return nil
}

func (f *fakeContainerResourceClient) Delete(_ context.Context, gvr schema.GroupVersionResource, _ string, name string) error {
	f.deleted = append(f.deleted, gvr.Resource+"/"+name)
	return f.deleteErr
}

func (f *fakeContainerResourceClient) ListByLabel(_ context.Context, gvr schema.GroupVersionResource, _ string, selector string) ([]unstructured.Unstructured, error) {
	if gvr != configMapsGVR || selector != containerAnalysisBundleSelector {
		return nil, errors.New("unexpected list")
	}
	return f.listed, f.listErr
}

func (f *fakeContainerResourceClient) TaskState(_ context.Context, _, name string) (TaskState, error) {
	if f.taskErr != nil {
		return TaskState{}, f.taskErr
	}
	return f.taskStates[name], nil
}

func lifecycleResources() ContainerAnalysisResources {
	return ContainerAnalysisResources{
		BundleConfigMap: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "bundle", "namespace": "orka-system"},
		},
		Task: map[string]any{
			"apiVersion": "core.orka.ai/v1alpha1", "kind": "Task",
			"metadata": map[string]any{"name": "task", "namespace": "orka-system"},
		},
	}
}

func TestReconcileContainerAnalysisResourcesPrunesAndAppliesInOrder(t *testing.T) {
	now := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	client := &fakeContainerResourceClient{
		listed: []unstructured.Unstructured{
			bundleObject("old", "old-task", now.Add(-ContainerAnalysisBundleRetention-time.Minute)),
			bundleObject("running", "running-task", now.Add(-2*ContainerAnalysisBundleRetention)),
			bundleObject("missing", "missing-task", now.Add(-2*ContainerAnalysisBundleRetention)),
			bundleObject("recent", "recent-task", now.Add(-time.Hour)),
			bundleObject("legacy", "", now.Add(-2*ContainerAnalysisBundleRetention)),
			bundleObject("unknown", "unknown-task", time.Time{}),
		},
		taskStates: map[string]TaskState{
			"old-task":     {Exists: true, Phase: "Succeeded"},
			"running-task": {Exists: true, Phase: "Running"},
		},
	}
	if err := ReconcileContainerAnalysisResources(context.Background(), client, lifecycleResources(), now); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(client.deleted, []string{"configmaps/old", "configmaps/missing", "configmaps/legacy"}) {
		t.Fatalf("deleted = %v", client.deleted)
	}
	if !reflect.DeepEqual(client.applied, []string{"configmaps/bundle", "tasks/task"}) {
		t.Fatalf("applied = %v", client.applied)
	}
}

func TestApplyContainerAnalysisResourcesRollsBackBundle(t *testing.T) {
	client := &fakeContainerResourceClient{applyErrAt: 2}
	err := ApplyContainerAnalysisResources(context.Background(), client, lifecycleResources())
	if err == nil || !strings.Contains(err.Error(), "apply container analysis Task") {
		t.Fatalf("ApplyContainerAnalysisResources error = %v", err)
	}
	if !reflect.DeepEqual(client.deleted, []string{"configmaps/bundle"}) {
		t.Fatalf("deleted = %v", client.deleted)
	}
}

func TestCleanupContainerAnalysisBundle(t *testing.T) {
	client := &fakeContainerResourceClient{}
	if err := CleanupContainerAnalysisBundle(context.Background(), client, lifecycleResources()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(client.deleted, []string{"configmaps/bundle"}) {
		t.Fatalf("deleted = %v", client.deleted)
	}
}

func TestPruneContainerAnalysisBundlesFailsClosed(t *testing.T) {
	client := &fakeContainerResourceClient{listErr: errors.New("forbidden")}
	if _, err := PruneContainerAnalysisBundles(context.Background(), client, "orka-system", time.Now()); err == nil || !strings.Contains(err.Error(), "list") {
		t.Fatalf("PruneContainerAnalysisBundles error = %v", err)
	}
	client = &fakeContainerResourceClient{
		listed:     []unstructured.Unstructured{bundleObject("old", "old-task", time.Now().Add(-2*ContainerAnalysisBundleRetention))},
		taskStates: map[string]TaskState{"old-task": {Exists: true, Phase: "Failed"}},
		deleteErr:  errors.New("forbidden"),
	}
	if _, err := PruneContainerAnalysisBundles(context.Background(), client, "orka-system", time.Now()); err == nil || !strings.Contains(err.Error(), "delete expired") {
		t.Fatalf("PruneContainerAnalysisBundles error = %v", err)
	}
	client = &fakeContainerResourceClient{
		listed:  []unstructured.Unstructured{bundleObject("old", "old-task", time.Now().Add(-2*ContainerAnalysisBundleRetention))},
		taskErr: errors.New("forbidden"),
	}
	if _, err := PruneContainerAnalysisBundles(context.Background(), client, "orka-system", time.Now()); err == nil || !strings.Contains(err.Error(), "read Task") {
		t.Fatalf("PruneContainerAnalysisBundles error = %v", err)
	}
}

func bundleObject(name, taskName string, created time.Time) unstructured.Unstructured {
	object := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name": name, "namespace": "orka-system",
			"annotations": map[string]any{containerAnalysisTaskNameAnnotation: taskName},
		},
	}}
	if !created.IsZero() {
		object.SetCreationTimestamp(metav1.NewTime(created))
	}
	return object
}
