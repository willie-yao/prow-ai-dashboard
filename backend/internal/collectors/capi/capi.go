// This file wires the CAPI artifact discovery helpers in this package into
// the collectors.Collector interface used by cmd/fetcher. The cluster name
// prefix (e.g. "capz-e2e") comes from project.yaml.
package capi

import (
	"context"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// Collector implements the CAPI artifact layout: each failed build has zero or
// more cluster directories under artifacts/clusters/, and tests are mapped to
// clusters via the generic prefix matcher with the CAPZ-flavored rules table
// as a fallback. Controller logs from the management cluster (the kind
// "bootstrap" cluster) are discovered for each namespace declared in
// project.yaml's ai.evidence.controller_logs.
type Collector struct {
	bucket             *gcs.Bucket
	client             *http.Client
	clusterPrefix      string
	nsPrefixRe         *regexp.Regexp
	controllerLogs     []project.ControllerLogSelector
	controllerPodRegex []*regexp.Regexp
}

// New constructs a CAPI collector. clusterPrefix is optional. When non-empty
// (e.g. "capz-e2e"), the collector parses the build log for "Creating
// namespace" lines to map tests to per-test namespaces, and extracts a
// namespace prefix from each cluster name for the bootstrap-resources URL.
// When empty, the collector treats each cluster directory name as the
// namespace directly (the CAPI core convention, where one cluster per test
// has cluster_name == namespace).
//
// controllerLogs and controllerPodRegex are the parallel slices produced by
// project.Config.EffectiveEvidence (selectors + their compiled pod-name
// regexes). They drive controller log discovery from the management cluster.
// Pass nil/nil to disable controller log discovery.
func New(bucket *gcs.Bucket, client *http.Client, clusterPrefix string, controllerLogs []project.ControllerLogSelector, controllerPodRegex []*regexp.Regexp) (*Collector, error) {
	c := &Collector{
		bucket:             bucket,
		client:             client,
		clusterPrefix:      clusterPrefix,
		controllerLogs:     controllerLogs,
		controllerPodRegex: controllerPodRegex,
	}
	if clusterPrefix != "" {
		c.nsPrefixRe = regexp.MustCompile(regexp.QuoteMeta(clusterPrefix) + `-[a-z0-9]+`)
	}
	return c, nil
}

// Name implements collectors.Collector.
func (*Collector) Name() string { return "capi" }

// CollectArtifacts implements collectors.Collector. It discovers cluster dirs
// under artifacts/clusters/, parses the build log for namespace mappings, and
// maps each failed test case to a cluster (or a namespace as fallback).
//
// This is a 1:1 port of the inline block that previously lived in
// cmd/fetcher/main.go.
func (c *Collector) CollectArtifacts(ctx context.Context, jobName, buildID string, result *models.BuildResult) error {
	if result == nil {
		return nil
	}
	// Skip pending or passing builds — only failed runs need artifact discovery.
	if result.Result == "PENDING" || result.Passed || result.TestsFailed == 0 {
		return nil
	}

	clusters, err := DiscoverClusters(ctx, c.client, c.bucket, jobName, buildID)
	if err != nil {
		// 404 is expected for jobs that don't produce cluster artifacts (e.g., AKS, conformance).
		// Only log non-404 errors.
		if !strings.Contains(err.Error(), "404") {
			log.Printf("    ⚠ %s/%s: artifact discovery failed: %v", jobName, buildID, err)
		}
	}

	// Fetch build log for namespace mapping (best-effort).
	var namespaceMap map[string]string
	buildLog, err := gcs.FetchRaw(ctx, c.client, result.BuildLogURL)
	if err != nil {
		log.Printf("    ⚠ %s/%s: failed to fetch build log for namespace mapping: %v", jobName, buildID, err)
	} else {
		namespaceMap = ParseNamespaceMap(buildLog, c.clusterPrefix)
		log.Printf("    📋 %s/%s: build log %d bytes, %d namespace mappings", jobName, buildID, len(buildLog), len(namespaceMap))
	}

	bootstrapPath := jobName + "/" + buildID + "/artifacts/clusters/bootstrap/resources/"

	// Discover management-cluster controller logs as declared by
	// project.yaml. Errors are logged but non-fatal; missing entries are
	// surfaced by the AI module via its "Configured but missing" footer.
	if len(c.controllerLogs) > 0 {
		urls, err := DiscoverControllerLogs(ctx, c.client, c.bucket, jobName, buildID, c.controllerLogs, c.controllerPodRegex)
		if err != nil {
			log.Printf("    ⚠ %s/%s: controller log discovery failed: %v", jobName, buildID, err)
		}
		if len(urls) > 0 {
			result.ControllerLogURLs = urls
		}
	}

	for i := range result.TestCases {
		if result.TestCases[i].Status != "failed" {
			continue
		}

		ca := MapTestToCluster(result.TestCases[i].Name, clusters)
		if ca != nil {
			if ns := c.bootstrapNamespace(ca.ClusterName); ns != "" {
				ca.BootstrapResourcesURL = c.bucket.WebURL(bootstrapPath + ns + "/")
			}
			result.TestCases[i].ClusterArtifacts = ca
		} else if namespaceMap != nil {
			// No workload cluster match — try namespace from build log.
			ns := FindNamespaceForTest(result.TestCases[i].Name, namespaceMap)
			if ns != "" {
				result.TestCases[i].ClusterArtifacts = &models.ClusterArtifacts{
					ClusterName:           ns,
					BootstrapResourcesURL: c.bucket.WebURL(bootstrapPath + ns + "/"),
				}
			}
		}
	}

	return nil
}

// bootstrapNamespace returns the path segment under
// .../artifacts/clusters/bootstrap/resources/ that holds resource YAMLs for
// the given cluster. With a configured prefix the segment is the extracted
// namespace prefix (CAPZ: per-test namespace under one cluster); without
// one the segment is the full cluster name (CAPI core: one dir per cluster
// whose name is the namespace). Returns "" when no segment can be derived.
func (c *Collector) bootstrapNamespace(clusterName string) string {
	if clusterName == "" {
		return ""
	}
	if c.nsPrefixRe != nil {
		return c.nsPrefixRe.FindString(clusterName)
	}
	return clusterName
}
