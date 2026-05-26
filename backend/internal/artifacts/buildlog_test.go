package artifacts

import (
	"testing"
)

const sampleBuildLog = `INFO: "With 3 control-plane nodes and 2 Linux and 2 Windows worker nodes" started at Fri, 20 Mar 2026 16:32:57 UTC
STEP: Creating namespace "capz-e2e-5vz277" for hosting the cluster @ 03/20/26 16:32:57.18
INFO: Creating namespace capz-e2e-5vz277
INFO: "With Flatcar control-plane and worker nodes" started at Fri, 20 Mar 2026 16:32:57 UTC
STEP: Creating namespace "capz-e2e-f9snsp" for hosting the cluster @ 03/20/26 16:32:57.18
INFO: Creating namespace capz-e2e-f9snsp
INFO: "With ipv6 worker node" started at Fri, 20 Mar 2026 16:32:57 UTC
some intermediate log line
another line
STEP: Creating namespace "capz-e2e-tqpttw" for hosting the cluster @ 03/20/26 16:32:57.18
`

func TestParseNamespaceMap(t *testing.T) {
	t.Run("three test fragments with namespace pairs", func(t *testing.T) {
		nsMap := ParseNamespaceMap([]byte(sampleBuildLog))

		expected := map[string]string{
			"with 3 control-plane nodes and 2 linux and 2 windows worker nodes": "capz-e2e-5vz277",
			"with flatcar control-plane and worker nodes":                       "capz-e2e-f9snsp",
			"with ipv6 worker node":                                             "capz-e2e-tqpttw",
		}

		if len(nsMap) != len(expected) {
			t.Fatalf("expected %d entries, got %d: %v", len(expected), len(nsMap), nsMap)
		}
		for k, v := range expected {
			if nsMap[k] != v {
				t.Errorf("key %q: expected %q, got %q", k, v, nsMap[k])
			}
		}
	})

	t.Run("lines between INFO and STEP", func(t *testing.T) {
		input := `INFO: "Some test name" started at Fri, 20 Mar 2026 16:32:57 UTC
unrelated log line 1
unrelated log line 2
STEP: Creating namespace "capz-e2e-abc123" for hosting the cluster @ 03/20/26 16:32:57.18
`
		nsMap := ParseNamespaceMap([]byte(input))
		if ns, ok := nsMap["some test name"]; !ok || ns != "capz-e2e-abc123" {
			t.Errorf("expected capz-e2e-abc123 for 'some test name', got %q (ok=%v)", ns, ok)
		}
	})

	t.Run("empty build log", func(t *testing.T) {
		nsMap := ParseNamespaceMap([]byte(""))
		if len(nsMap) != 0 {
			t.Errorf("expected empty map, got %v", nsMap)
		}
	})

	t.Run("malformed lines", func(t *testing.T) {
		input := `this is not an INFO line
STEP: no match here either
INFO: missing quotes started at something
STEP: Creating namespace "not-capz-prefix" for hosting the cluster
`
		nsMap := ParseNamespaceMap([]byte(input))
		if len(nsMap) != 0 {
			t.Errorf("expected empty map for malformed input, got %v", nsMap)
		}
	})
}

func TestFindNamespaceForTest(t *testing.T) {
	nsMap := map[string]string{
		"with 3 control-plane nodes and 2 linux and 2 windows worker nodes": "capz-e2e-5vz277",
		"with flatcar control-plane and worker nodes":                       "capz-e2e-f9snsp",
		"with ipv6 worker node":                                             "capz-e2e-tqpttw",
	}

	t.Run("exact suffix match", func(t *testing.T) {
		testName := "[It] Workload cluster creation Creating a highly-available cluster [REQUIRED] With 3 control-plane nodes and 2 Linux and 2 Windows worker nodes"
		ns := FindNamespaceForTest(testName, nsMap)
		if ns != "capz-e2e-5vz277" {
			t.Errorf("expected capz-e2e-5vz277, got %q", ns)
		}
	})

	t.Run("case insensitive match", func(t *testing.T) {
		testName := "WITH FLATCAR CONTROL-PLANE AND WORKER NODES"
		ns := FindNamespaceForTest(testName, nsMap)
		if ns != "capz-e2e-f9snsp" {
			t.Errorf("expected capz-e2e-f9snsp, got %q", ns)
		}
	})

	t.Run("no match returns empty", func(t *testing.T) {
		ns := FindNamespaceForTest("completely unrelated test name", nsMap)
		if ns != "" {
			t.Errorf("expected empty string, got %q", ns)
		}
	})

	t.Run("picks correct fragment among multiple", func(t *testing.T) {
		testName := "[It] Creating an IPv6 cluster With ipv6 worker node"
		ns := FindNamespaceForTest(testName, nsMap)
		if ns != "capz-e2e-tqpttw" {
			t.Errorf("expected capz-e2e-tqpttw, got %q", ns)
		}
	})
}

func TestDebugRealBuildLog(t *testing.T) {
    t.Skip("manual debug test")
}
