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
	applyErrAt   int
	createExists bool
	createErr    error
	getErr       error
	deleteErr    error
	listErr      error
	taskErr      error
	applied      []string
	created      []string
	deleted      []string
	listed       []unstructured.Unstructured
	existing     *unstructured.Unstructured
	taskStates   map[string]TaskState
}

func (f *fakeContainerResourceClient) Apply(_ context.Context, gvr schema.GroupVersionResource, _ string, obj map[string]any) error {
	name := (&unstructured.Unstructured{Object: obj}).GetName()
	f.applied = append(f.applied, gvr.Resource+"/"+name)
	if f.applyErrAt > 0 && len(f.applied) == f.applyErrAt {
		return errors.New("apply failed")
	}
	return nil
}

func (f *fakeContainerResourceClient) CreateIfAbsent(_ context.Context, gvr schema.GroupVersionResource, _ string, obj map[string]any) (bool, error) {
	name := (&unstructured.Unstructured{Object: obj}).GetName()
	f.created = append(f.created, gvr.Resource+"/"+name)
	if f.createErr != nil {
		return false, f.createErr
	}
	return !f.createExists, nil
}

func (f *fakeContainerResourceClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	return f.existing, f.getErr
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
			"apiVersion": "v1", "kind": "ConfigMap", "immutable": true,
			"metadata": map[string]any{
				"name": "bundle", "namespace": "orka-system",
				"labels": map[string]any{containerAnalysisBundleLabel: "true"},
				"annotations": map[string]any{
					"prow-ai-dashboard/bundle-digest":    "digest",
					"prow-ai-dashboard/contract-version": ContainerAnalysisContractVersion,
					containerAnalysisTaskNameAnnotation:  "task",
				},
			},
			"data": map[string]any{"bundle.json": "{}"},
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
	if !reflect.DeepEqual(client.created, []string{"configmaps/bundle"}) {
		t.Fatalf("created = %v", client.created)
	}
	if !reflect.DeepEqual(client.applied, []string{"tasks/task"}) {
		t.Fatalf("applied = %v", client.applied)
	}
}

func TestApplyContainerAnalysisResourcesRollsBackOnlyNewBundle(t *testing.T) {
	resources := lifecycleResources()
	client := &fakeContainerResourceClient{applyErrAt: 1}
	err := ApplyContainerAnalysisResources(context.Background(), client, resources)
	if err == nil || !strings.Contains(err.Error(), "apply container analysis Task") {
		t.Fatalf("ApplyContainerAnalysisResources error = %v", err)
	}
	if !reflect.DeepEqual(client.deleted, []string{"configmaps/bundle"}) {
		t.Fatalf("deleted = %v", client.deleted)
	}

	client = &fakeContainerResourceClient{
		applyErrAt:   1,
		createExists: true,
		existing:     &unstructured.Unstructured{Object: resources.BundleConfigMap},
	}
	err = ApplyContainerAnalysisResources(context.Background(), client, resources)
	if err == nil || !strings.Contains(err.Error(), "apply container analysis Task") {
		t.Fatalf("ApplyContainerAnalysisResources error = %v", err)
	}
	if len(client.deleted) != 0 {
		t.Fatalf("existing bundle was deleted: %v", client.deleted)
	}
}

func TestApplyContainerAnalysisResourcesRejectsMismatchedExistingBundle(t *testing.T) {
	resources := lifecycleResources()
	existing := &unstructured.Unstructured{Object: map[string]any{}}
	existing.Object = deepCopyMap(resources.BundleConfigMap)
	existing.Object["data"].(map[string]any)["bundle.json"] = "different"
	client := &fakeContainerResourceClient{createExists: true, existing: existing}
	if err := ApplyContainerAnalysisResources(context.Background(), client, resources); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("ApplyContainerAnalysisResources error = %v", err)
	}
	if len(client.applied) != 0 || len(client.deleted) != 0 {
		t.Fatalf("mismatched existing bundle changed resources: applied=%v deleted=%v", client.applied, client.deleted)
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

func deepCopyMap(in map[string]any) map[string]any {
	return (&unstructured.Unstructured{Object: in}).DeepCopy().Object
}
