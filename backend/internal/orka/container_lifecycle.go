package orka

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var configMapsGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

const (
	containerAnalysisBundleLabel        = "prow-ai-dashboard/bundle"
	containerAnalysisBundleSelector     = containerAnalysisBundleLabel + "=true"
	containerAnalysisTaskNameAnnotation = "prow-ai-dashboard/task-name"
	// ContainerAnalysisBundleRetention bounds orphaned private input bundles.
	ContainerAnalysisBundleRetention = 24 * time.Hour
)

// ContainerAnalysisResourceClient applies and removes analyzer resources.
type ContainerAnalysisResourceClient interface {
	Apply(context.Context, schema.GroupVersionResource, string, map[string]any) error
	Delete(context.Context, schema.GroupVersionResource, string, string) error
	ListByLabel(context.Context, schema.GroupVersionResource, string, string) ([]unstructured.Unstructured, error)
	TaskState(context.Context, string, string) (TaskState, error)
}

// ReconcileContainerAnalysisResources prunes stale bundles, then applies a bundle before its Task.
func ReconcileContainerAnalysisResources(ctx context.Context, client ContainerAnalysisResourceClient, resources ContainerAnalysisResources, now time.Time) error {
	namespace, _, err := containerResourceRef(resources.BundleConfigMap)
	if err != nil {
		return err
	}
	if _, err := PruneContainerAnalysisBundles(ctx, client, namespace, now); err != nil {
		return err
	}
	if err := ApplyContainerAnalysisResources(ctx, client, resources); err != nil {
		return err
	}
	return nil
}

// ApplyContainerAnalysisResources applies the bundle before its Task and rolls back on failure.
func ApplyContainerAnalysisResources(ctx context.Context, client ContainerAnalysisResourceClient, resources ContainerAnalysisResources) error {
	bundleNamespace, bundleName, err := containerResourceRef(resources.BundleConfigMap)
	if err != nil {
		return err
	}
	taskNamespace, _, err := containerResourceRef(resources.Task)
	if err != nil {
		return err
	}
	if taskNamespace != bundleNamespace {
		return fmt.Errorf("container analysis Task and bundle namespaces differ")
	}
	if err := client.Apply(ctx, configMapsGVR, bundleNamespace, resources.BundleConfigMap); err != nil {
		return fmt.Errorf("apply container analysis bundle %s: %w", bundleName, err)
	}
	if err := client.Apply(ctx, TasksGVR, taskNamespace, resources.Task); err != nil {
		applyErr := fmt.Errorf("apply container analysis Task: %w", err)
		if cleanupErr := client.Delete(ctx, configMapsGVR, bundleNamespace, bundleName); cleanupErr != nil {
			return errors.Join(applyErr, fmt.Errorf("roll back container analysis bundle %s: %w", bundleName, cleanupErr))
		}
		return applyErr
	}
	return nil
}

// CleanupContainerAnalysisBundle deletes private inputs after terminal result handling.
func CleanupContainerAnalysisBundle(ctx context.Context, client ContainerAnalysisResourceClient, resources ContainerAnalysisResources) error {
	namespace, name, err := containerResourceRef(resources.BundleConfigMap)
	if err != nil {
		return err
	}
	if err := client.Delete(ctx, configMapsGVR, namespace, name); err != nil {
		return fmt.Errorf("delete container analysis bundle %s: %w", name, err)
	}
	return nil
}

// PruneContainerAnalysisBundles removes orphaned bundles older than the retention window.
func PruneContainerAnalysisBundles(ctx context.Context, client ContainerAnalysisResourceClient, namespace string, now time.Time) (int, error) {
	if namespace == "" {
		return 0, fmt.Errorf("container analysis bundle namespace is required")
	}
	items, err := client.ListByLabel(ctx, configMapsGVR, namespace, containerAnalysisBundleSelector)
	if err != nil {
		return 0, fmt.Errorf("list container analysis bundles: %w", err)
	}
	cutoff := now.Add(-ContainerAnalysisBundleRetention)
	deleted := 0
	for i := range items {
		created := items[i].GetCreationTimestamp()
		if created.IsZero() || created.Time.After(cutoff) {
			continue
		}
		taskName := items[i].GetAnnotations()[containerAnalysisTaskNameAnnotation]
		if taskName != "" {
			state, err := client.TaskState(ctx, namespace, taskName)
			if err != nil {
				return deleted, fmt.Errorf("read Task for expired container analysis bundle %s: %w", items[i].GetName(), err)
			}
			if state.Exists && !TerminalPhase(state.Phase) {
				continue
			}
		}
		if err := client.Delete(ctx, configMapsGVR, namespace, items[i].GetName()); err != nil {
			return deleted, fmt.Errorf("delete expired container analysis bundle %s: %w", items[i].GetName(), err)
		}
		deleted++
	}
	return deleted, nil
}

func containerResourceRef(resource map[string]any) (string, string, error) {
	object := &unstructured.Unstructured{Object: resource}
	if object.GetNamespace() == "" || object.GetName() == "" {
		return "", "", fmt.Errorf("container analysis resource requires namespace and name")
	}
	return object.GetNamespace(), object.GetName(), nil
}
