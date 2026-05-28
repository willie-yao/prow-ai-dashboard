// Controller log discovery for management-cluster controllers (the
// "bootstrap" kind cluster's pods). For each consumer-declared namespace
// selector, this walker enumerates all deployments under that namespace
// and records one container-log URL per deployment, keyed by
// "<namespace>/<deployment>" on BuildResult.ControllerLogURLs.
//
// Layout under GCS for a single build:
//   logs/<jobName>/<buildID>/artifacts/clusters/bootstrap/logs/
//     <namespace>/<deployment>/<pod>/<container_log>
//
// A single namespace often runs multiple deployments (e.g. CAPZ's
// capz-system contains both Azure Service Operator and the CAPZ
// controller). Picking only one per namespace would silently drop
// signal, so we record one per deployment.
package capi

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// DiscoverControllerLogs lists every controller log object under each
// configured namespace and returns a map keyed by "<namespace>/<deployment>"
// of one selected container-log URL per deployment. Pod filtering uses each
// selector's pre-compiled PodNameRegex. Selectors with empty namespaces are
// skipped (project.Validate rejects them at load time, this guards against
// programmer error). Returns nil with no error if no selectors are
// configured. A 404 or empty result is not an error; missing logs are
// surfaced to the AI prompt later via the RequestedButMissing footer.
func DiscoverControllerLogs(
	ctx context.Context,
	client *http.Client,
	bucket *gcs.Bucket,
	jobName, buildID string,
	selectors []project.ControllerLogSelector,
	podRegexes []*regexp.Regexp,
) (map[string]string, error) {
	if len(selectors) == 0 {
		return nil, nil
	}

	result := make(map[string]string)
	for i, sel := range selectors {
		if sel.Namespace == "" {
			continue
		}
		// Use the parallel pre-compiled regex; fall back to defaultPodNameRegex
		// if the slice is shorter for any reason.
		var podRe *regexp.Regexp
		if i < len(podRegexes) {
			podRe = podRegexes[i]
		}

		containerLog := sel.ContainerLog
		if containerLog == "" {
			containerLog = "manager.log"
		}

		// Object prefix to list. The trailing slash is critical so a
		// "capi-system" selector doesn't accidentally match
		// "capi-system-extras" later if such a namespace ever appears.
		prefix := "logs/" + jobName + "/" + buildID +
			"/artifacts/clusters/bootstrap/logs/" + sel.Namespace + "/"

		names, err := gcs.ListObjects(ctx, client, bucket.ListAPIURL(), prefix)
		if err != nil {
			// Don't fail the whole build for one namespace listing error.
			// The AI module will surface the gap via RequestedButMissing.
			continue
		}

		// Walk every object name and pick one matching log per deployment.
		// Sort first so the chosen pod is deterministic across runs.
		sort.Strings(names)

		matches := selectControllerLogObjects(names, prefix, podRe, containerLog)
		for _, m := range matches {
			key := sel.Namespace + "/" + m.deployment
			if _, exists := result[key]; exists {
				continue
			}
			// ListObjects returns the bucket-relative path including
			// "logs/" prefix; strip it because Bucket.ObjectURL re-adds
			// "logs/" for us.
			withoutLogsPrefix := strings.TrimPrefix(m.objectName, "logs/")
			result[key] = bucket.ObjectURL(withoutLogsPrefix)
		}
	}

	return result, nil
}

// controllerLogMatch is one (deployment, pod, full object name) triple
// extracted from a GCS object listing. Kept package-internal so tests can
// exercise the parser directly without needing a real GCS bucket.
type controllerLogMatch struct {
	deployment string
	pod        string
	objectName string
}

// selectControllerLogObjects filters a list of GCS object names (already
// listed under prefix) down to one container-log entry per deployment.
// names must be pre-sorted so output is deterministic.
//
// Layout assumption: prefix + "<deployment>/<pod>/<containerLog>" exactly.
// Names with additional path segments (e.g. a nested subdir) are ignored.
// podRe may be nil (no filter).
func selectControllerLogObjects(names []string, prefix string, podRe *regexp.Regexp, containerLog string) []controllerLogMatch {
	var out []controllerLogMatch
	seen := make(map[string]struct{})
	for _, name := range names {
		rest := strings.TrimPrefix(name, prefix)
		if rest == name {
			continue
		}
		parts := strings.Split(rest, "/")
		if len(parts) != 3 {
			continue
		}
		deployment, pod, file := parts[0], parts[1], parts[2]
		if file != containerLog {
			continue
		}
		if podRe != nil && !podRe.MatchString(pod) {
			continue
		}
		if _, dup := seen[deployment]; dup {
			continue
		}
		seen[deployment] = struct{}{}
		out = append(out, controllerLogMatch{deployment, pod, name})
	}
	return out
}
