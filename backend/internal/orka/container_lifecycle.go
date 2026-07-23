package orka

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var configMapsGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

const (
	containerAnalysisBundleLabel        = "prow-ai-dashboard/bundle"
	containerAnalysisBundleSelector     = containerAnalysisBundleLabel + "=true"
	containerAnalysisTaskNameAnnotation = "prow-ai-dashboard/task-name"
	containerAnalysisClaimAnnotation    = "prow-ai-dashboard/bundle-claim"
	// ContainerAnalysisBundleRetention bounds orphaned private input bundles.
	ContainerAnalysisBundleRetention = 24 * time.Hour
)

// ContainerAnalysisResourceClient applies and removes analyzer resources.
type ContainerAnalysisResourceClient interface {
	Apply(context.Context, schema.GroupVersionResource, string, map[string]any) error
	CreateIfAbsent(context.Context, schema.GroupVersionResource, string, map[string]any) (bool, error)
	Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error)
	PatchAnnotations(context.Context, schema.GroupVersionResource, string, string, map[string]string) (string, error)
	Delete(context.Context, schema.GroupVersionResource, string, string) error
	DeleteIfResourceVersion(context.Context, schema.GroupVersionResource, string, string, string) (bool, error)
	ListByLabel(context.Context, schema.GroupVersionResource, string, string) ([]unstructured.Unstructured, error)
	TaskState(context.Context, string, string) (TaskState, error)
}

// ReconcileContainerAnalysisResources prunes stale bundles, then applies a bundle before its Task.
func ReconcileContainerAnalysisResources(ctx context.Context, client ContainerAnalysisResourceClient, resources ContainerAnalysisResources, now time.Time) error {
	namespace, _, err := containerResourceRef(resources.BundleConfigMap)
	if err != nil {
		return err
	}
	taskNamespace, taskName, err := containerResourceRef(resources.Task)
	if err != nil {
		return err
	}
	if taskNamespace != namespace {
		return fmt.Errorf("container analysis Task and bundle namespaces differ")
	}
	if _, err := PruneContainerAnalysisBundles(ctx, client, namespace, now); err != nil {
		return err
	}
	state, err := client.TaskState(ctx, taskNamespace, taskName)
	if err != nil {
		return fmt.Errorf("read container analysis Task %s: %w", taskName, err)
	}
	if state.Exists && TerminalPhase(state.Phase) {
		return CleanupContainerAnalysisBundle(ctx, client, resources)
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
	taskNamespace, taskName, err := containerResourceRef(resources.Task)
	if err != nil {
		return err
	}
	if taskNamespace != bundleNamespace {
		return fmt.Errorf("container analysis Task and bundle namespaces differ")
	}
	claim, err := newContainerAnalysisBundleClaim()
	if err != nil {
		return err
	}
	created, err := client.CreateIfAbsent(ctx, configMapsGVR, bundleNamespace, resources.BundleConfigMap)
	if err != nil {
		return fmt.Errorf("create container analysis bundle %s: %w", bundleName, err)
	}
	existing, err := client.Get(ctx, configMapsGVR, bundleNamespace, bundleName)
	if err != nil {
		return fmt.Errorf("read container analysis bundle %s: %w", bundleName, err)
	}
	if err := validateExistingContainerAnalysisBundle(existing, resources.BundleConfigMap); err != nil {
		return err
	}
	claimedVersion, err := client.PatchAnnotations(ctx, configMapsGVR, bundleNamespace, bundleName, map[string]string{
		containerAnalysisClaimAnnotation: claim,
	})
	if err != nil {
		if created {
			_ = rollbackContainerAnalysisBundle(ctx, client, bundleNamespace, bundleName, taskName, existing.GetResourceVersion())
		}
		return fmt.Errorf("claim container analysis bundle %s: %w", bundleName, err)
	}
	if err := client.Apply(ctx, TasksGVR, taskNamespace, resources.Task); err != nil {
		applyErr := fmt.Errorf("apply container analysis Task: %w", err)
		if !created {
			return applyErr
		}
		if cleanupErr := rollbackContainerAnalysisBundle(ctx, client, bundleNamespace, bundleName, taskName, claimedVersion); cleanupErr != nil {
			return errors.Join(applyErr, cleanupErr)
		}
		return applyErr
	}
	return nil
}

func rollbackContainerAnalysisBundle(ctx context.Context, client ContainerAnalysisResourceClient, namespace, bundleName, taskName, resourceVersion string) error {
	state, err := client.TaskState(ctx, namespace, taskName)
	if err != nil {
		return fmt.Errorf("check Task before rolling back container analysis bundle %s: %w", bundleName, err)
	}
	if state.Exists {
		return nil
	}
	if _, err := client.DeleteIfResourceVersion(ctx, configMapsGVR, namespace, bundleName, resourceVersion); err != nil {
		return fmt.Errorf("roll back container analysis bundle %s: %w", bundleName, err)
	}
	return nil
}

func newContainerAnalysisBundleClaim() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create container analysis bundle claim: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func validateExistingContainerAnalysisBundle(existing *unstructured.Unstructured, expected map[string]any) error {
	if existing == nil {
		return fmt.Errorf("existing container analysis bundle is missing")
	}
	expectedObject := &unstructured.Unstructured{Object: expected}
	immutable, found, err := unstructured.NestedBool(existing.Object, "immutable")
	if err != nil || !found || !immutable {
		return fmt.Errorf("existing container analysis bundle %s is not immutable", existing.GetName())
	}
	existingData, found, err := unstructured.NestedStringMap(existing.Object, "data")
	if err != nil || !found {
		return fmt.Errorf("existing container analysis bundle %s has invalid data", existing.GetName())
	}
	expectedData, found, err := unstructured.NestedStringMap(expectedObject.Object, "data")
	if err != nil || !found || !maps.Equal(existingData, expectedData) {
		return fmt.Errorf("existing container analysis bundle %s does not match the requested content", existing.GetName())
	}
	for _, key := range []string{"prow-ai-dashboard/bundle-digest", "prow-ai-dashboard/contract-version", containerAnalysisTaskNameAnnotation} {
		if existing.GetAnnotations()[key] != expectedObject.GetAnnotations()[key] {
			return fmt.Errorf("existing container analysis bundle %s has mismatched identity", existing.GetName())
		}
	}
	if existing.GetLabels()[containerAnalysisBundleLabel] != "true" {
		return fmt.Errorf("existing container analysis bundle %s is missing the retention label", existing.GetName())
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
		resourceVersion := items[i].GetResourceVersion()
		if resourceVersion == "" {
			continue
		}
		removed, err := client.DeleteIfResourceVersion(ctx, configMapsGVR, namespace, items[i].GetName(), resourceVersion)
		if err != nil {
			return deleted, fmt.Errorf("delete expired container analysis bundle %s: %w", items[i].GetName(), err)
		}
		if removed {
			deleted++
		}
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
