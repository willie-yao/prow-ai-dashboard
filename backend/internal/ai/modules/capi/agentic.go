package capi

import (
	"fmt"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// PrefersAgentic returns true for failures where the curator pipeline has
// little or nothing useful to send to the model. In those cases the agentic
// tool-calling loop, which browses the build's artifact tree on demand, can
// reliably do better than a fixed evidence schema.
//
// Triggers (any one is enough):
//   - No ClusterArtifacts at all. The collector never matched a cluster to
//     this test, which usually means the failure happened before any cluster
//     was created (e.g. pre-flight check, bootstrap failure, ginkgo panic).
//     Curator falls back to the bare build-log; agentic can hunt through
//     artifacts/ for whatever the runner did emit.
//   - ClusterArtifacts is present but its Machines list AND PodLogDirs map
//     are both empty. The cluster name is known but no per-machine or
//     per-controller logs were collected, so the curator's machine_logs /
//     controller_logs sections are all empty — same situation as no
//     ClusterArtifacts, just discovered later in the pipeline.
//
// Returning a non-empty reason gives operators a single-line explanation in
// fetcher logs for why a particular failure went through the more expensive
// agentic pipeline. Modules with no curator deficit return (false, "") and
// let the project's Agentic.Always setting decide.
func (m *Module) PrefersAgentic(_ *models.BuildResult, tc *models.TestCase) (bool, string) {
	if tc == nil {
		return false, ""
	}
	if tc.ClusterArtifacts == nil {
		return true, "no cluster artifacts collected; curator has only the build log"
	}
	ca := tc.ClusterArtifacts
	if len(ca.Machines) == 0 && len(ca.PodLogDirs) == 0 {
		return true, fmt.Sprintf("cluster %q has no machine or controller logs collected", ca.ClusterName)
	}
	return false, ""
}
