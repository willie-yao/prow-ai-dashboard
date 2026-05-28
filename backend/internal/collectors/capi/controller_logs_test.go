package capi

import (
	"reflect"
	"regexp"
	"testing"
)

// TestSelectControllerLogObjects_RealLayouts feeds the parser the actual
// object-name shapes observed under CAPZ and CAPI core bootstrap clusters
// and verifies one entry per deployment is selected.
func TestSelectControllerLogObjects_RealLayouts(t *testing.T) {
	prefix := "logs/periodic-job/123/artifacts/clusters/bootstrap/logs/capz-system/"
	names := []string{
		prefix + "azureserviceoperator-controller-manager/azureserviceoperator-controller-manager-6456c6f58c-bsrj8/manager-log-metadata.json",
		prefix + "azureserviceoperator-controller-manager/azureserviceoperator-controller-manager-6456c6f58c-bsrj8/manager.log",
		prefix + "azureserviceoperator-controller-manager/azureserviceoperator-controller-manager-6456c6f58c-d6wdv/manager-log-metadata.json",
		prefix + "azureserviceoperator-controller-manager/azureserviceoperator-controller-manager-6456c6f58c-d6wdv/manager.log",
		prefix + "capz-controller-manager/capz-controller-manager-f67744496-p9sg6/manager-log-metadata.json",
		prefix + "capz-controller-manager/capz-controller-manager-f67744496-p9sg6/manager.log",
	}

	got := selectControllerLogObjects(names, prefix, nil, "manager.log")

	want := []controllerLogMatch{
		{
			deployment: "azureserviceoperator-controller-manager",
			pod:        "azureserviceoperator-controller-manager-6456c6f58c-bsrj8",
			objectName: prefix + "azureserviceoperator-controller-manager/azureserviceoperator-controller-manager-6456c6f58c-bsrj8/manager.log",
		},
		{
			deployment: "capz-controller-manager",
			pod:        "capz-controller-manager-f67744496-p9sg6",
			objectName: prefix + "capz-controller-manager/capz-controller-manager-f67744496-p9sg6/manager.log",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("selectControllerLogObjects mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// TestSelectControllerLogObjects_PodRegexFilter verifies that the pod-name
// regex narrows the match set: only deployments whose pods match are kept.
func TestSelectControllerLogObjects_PodRegexFilter(t *testing.T) {
	prefix := "logs/job/1/artifacts/clusters/bootstrap/logs/capz-system/"
	names := []string{
		prefix + "azureserviceoperator-controller-manager/azureserviceoperator-controller-manager-pod-a/manager.log",
		prefix + "capz-controller-manager/capz-controller-manager-pod-b/manager.log",
	}
	re := regexp.MustCompile(`^capz-controller-manager-.*`)

	got := selectControllerLogObjects(names, prefix, re, "manager.log")
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d: %#v", len(got), got)
	}
	if got[0].deployment != "capz-controller-manager" {
		t.Errorf("expected capz-controller-manager, got %q", got[0].deployment)
	}
}

// TestSelectControllerLogObjects_SkipsMetadata verifies that the parser
// only matches the configured container log name and ignores adjacent
// metadata files that happen to share the same pod directory.
func TestSelectControllerLogObjects_SkipsMetadata(t *testing.T) {
	prefix := "logs/job/1/artifacts/clusters/bootstrap/logs/capi-system/"
	names := []string{
		prefix + "capi-controller-manager/pod-x/manager-log-metadata.json",
		prefix + "capi-controller-manager/pod-x/manager.log",
	}
	got := selectControllerLogObjects(names, prefix, nil, "manager.log")
	if len(got) != 1 || got[0].objectName != names[1] {
		t.Errorf("expected only manager.log to match, got %#v", got)
	}
}

// TestSelectControllerLogObjects_NoMatchOnDeepNesting verifies that the
// parser ignores objects nested deeper than the expected 3-segment shape.
// This protects against any future GCS layout change that adds a nested
// directory.
func TestSelectControllerLogObjects_NoMatchOnDeepNesting(t *testing.T) {
	prefix := "logs/job/1/artifacts/clusters/bootstrap/logs/ns/"
	names := []string{
		prefix + "deployment/pod/extra/manager.log", // 4 segments, rejected
	}
	got := selectControllerLogObjects(names, prefix, nil, "manager.log")
	if len(got) != 0 {
		t.Errorf("expected 0 matches for deep-nested path, got %d: %#v", len(got), got)
	}
}
