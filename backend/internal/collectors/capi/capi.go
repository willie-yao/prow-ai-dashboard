// This file wires the CAPI artifact discovery helpers in this package into
// the collectors.Collector interface used by cmd/fetcher. The cluster name
// prefix (e.g. "capz-e2e") comes from project.yaml.
package capi

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Collector implements the CAPI artifact layout: each failed build has zero or
// more cluster directories under artifacts/clusters/, and tests are mapped to
// clusters via the clusterFlavorRules keyword table.
type Collector struct {
	bucket        *gcs.Bucket
	client        *http.Client
	clusterPrefix string
	nsPrefixRe    *regexp.Regexp
}

// New constructs a CAPI collector. clusterPrefix is required (e.g. "capz-e2e").
func New(bucket *gcs.Bucket, client *http.Client, clusterPrefix string) (*Collector, error) {
	if clusterPrefix == "" {
		return nil, fmt.Errorf("capi collector requires a non-empty cluster_name_prefix in project.yaml")
	}
	return &Collector{
		bucket:        bucket,
		client:        client,
		clusterPrefix: clusterPrefix,
		nsPrefixRe:    regexp.MustCompile(regexp.QuoteMeta(clusterPrefix) + `-[a-z0-9]+`),
	}, nil
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

	for i := range result.TestCases {
		if result.TestCases[i].Status != "failed" {
			continue
		}

		ca := MapTestToCluster(result.TestCases[i].Name, clusters)
		if ca != nil {
			// Add bootstrap resources URL by extracting namespace prefix.
			if prefix := c.nsPrefixRe.FindString(ca.ClusterName); prefix != "" {
				ca.BootstrapResourcesURL = c.bucket.WebURL(bootstrapPath + prefix + "/")
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
