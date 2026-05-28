// Package capi is the Cluster API artifact collector. It discovers per-cluster
// debug artifact directories for failed E2E test runs stored in GCS. The cluster
// naming convention (e.g. capz-e2e-{random}-{flavor}) comes from CAPI's e2e
// framework; the cluster prefix is configurable per project.
package capi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/net/html"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// knownMachineLogs lists the log file names we look for inside each machine directory,
// in priority order. boot.log is most useful as it aggregates other logs.
var knownMachineLogs = []string{
	"boot.log",
	"cloud-init-output.log",
	"cloud-init.log",
	"kubelet.log",
	"kube-apiserver.log",
	"journal.log",
	"kern.log",
	"containerd.log",
}

// DiscoverClusters fetches the GCSweb listing at .../artifacts/clusters/ for
// the given build, then inspects each cluster subdirectory to build a list of
// ClusterArtifacts (machines, activity logs, pod log dirs).
func DiscoverClusters(ctx context.Context, client *http.Client, bucket *gcs.Bucket, jobName, buildID string) ([]models.ClusterArtifacts, error) {
	buildPath := jobName + "/" + buildID + "/artifacts/clusters/"
	base := bucket.WebURL(buildPath)
	gcsBase := bucket.ObjectBaseURL(buildPath)

	return discoverClustersFromURL(ctx, client, base, gcsBase)
}

// discoverClustersFromURL is the testable core of DiscoverClusters,
// accepting arbitrary base URLs so tests can substitute an httptest server.
func discoverClustersFromURL(ctx context.Context, client *http.Client, listingBaseURL, gcsBaseURL string) ([]models.ClusterArtifacts, error) {
	dirs, err := fetchDirs(ctx, client, listingBaseURL)
	if err != nil {
		return nil, fmt.Errorf("listing clusters: %w", err)
	}

	var clusters []models.ClusterArtifacts
	for _, dir := range dirs {
		if strings.EqualFold(dir, "bootstrap") {
			continue
		}

		ca := models.ClusterArtifacts{ClusterName: dir}

		// Check for provider activity log by listing the azure-activity-logs dir.
		actLogDirs, err := fetchDirs(ctx, client, listingBaseURL+dir+"/azure-activity-logs/")
		if err == nil {
			// Activity logs are files, not dirs. Instead construct URL and leave it —
			// but only if the directory exists (fetchDirs succeeded).
			ca.ProviderActivityLog = gcsBaseURL + dir + "/azure-activity-logs/" + dir + ".log"
		}
		_ = actLogDirs

		// Discover machines and their actual log files.
		machinesURL := listingBaseURL + dir + "/machines/"
		machineNames, err := fetchDirs(ctx, client, machinesURL)
		if err == nil {
			for _, mn := range machineNames {
				ma := models.MachineArtifacts{
					Name: mn,
					Logs: make(map[string]string),
				}
				// List actual files in the machine dir to only link non-empty ones.
				machineFiles, ferr := fetchNonEmptyFiles(ctx, client, machinesURL+mn+"/")
				if ferr == nil {
					fileSet := make(map[string]bool)
					for _, f := range machineFiles {
						fileSet[f] = true
					}
					for _, logFile := range knownMachineLogs {
						if fileSet[logFile] {
							ma.Logs[logFile] = gcsBaseURL + dir + "/machines/" + mn + "/" + logFile
						}
					}
				} else {
					// Fallback: link all known logs without verification.
					for _, logFile := range knownMachineLogs {
						ma.Logs[logFile] = gcsBaseURL + dir + "/machines/" + mn + "/" + logFile
					}
				}
				if len(ma.Logs) > 0 {
					ca.Machines = append(ca.Machines, ma)
				}
			}
		}
		// Ignore errors listing machines — the dir may not exist.

		// Discover pod log directories (top-level dirs other than azure-activity-logs and machines).
		topDirs, err := fetchDirs(ctx, client, listingBaseURL+dir+"/")
		if err == nil {
			for _, td := range topDirs {
				lower := strings.ToLower(td)
				if lower == "machines" || lower == "azure-activity-logs" || lower == "nodes" {
					continue
				}
				if ca.PodLogDirs == nil {
					ca.PodLogDirs = make(map[string]string)
				}
				ca.PodLogDirs[td] = listingBaseURL + dir + "/" + td + "/"
			}
		}

		clusters = append(clusters, ca)
	}

	return clusters, nil
}

// fetchDirs fetches a GCSweb HTML listing page and returns the directory names found.
func fetchDirs(ctx context.Context, client *http.Client, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d for %s", resp.StatusCode, url)
	}

	return parseGCSWebDirs(resp.Body)
}

// fetchNonEmptyFiles fetches a GCSweb listing page and returns file names that have non-zero size.
func fetchNonEmptyFiles(ctx context.Context, client *http.Client, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d for %s", resp.StatusCode, url)
	}

	return parseGCSWebNonEmptyFiles(resp.Body)
}

// parseGCSWebNonEmptyFiles parses GCSweb HTML and returns file names with non-zero size.
// GCSweb rows are: <li><div>Name (with <a>)</div><div>Size</div><div>Modified</div></li>
// Files with size "0" or "-" (directories) are excluded.
func parseGCSWebNonEmptyFiles(r io.Reader) ([]string, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, fmt.Errorf("parsing HTML: %w", err)
	}

	var files []string
	var walkLI func(*html.Node)
	walkLI = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "li" {
			// Collect the div children.
			var divs []*html.Node
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode && c.Data == "div" {
					divs = append(divs, c)
				}
			}
			// Need at least 2 divs: name and size.
			if len(divs) >= 2 {
				name := extractLinkName(divs[0])
				size := textContent(divs[1])
				if name != "" && name != ".." && size != "-" && size != "0" && size != "" {
					files = append(files, name)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walkLI(c)
		}
	}
	walkLI(doc)
	return files, nil
}

// extractLinkName finds the first <a> in a node and returns the entry name from its href.
func extractLinkName(n *html.Node) string {
	var name string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, attr := range n.Attr {
				if attr.Key == "href" {
					name = extractEntryName(attr.Val)
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return name
}

// textContent returns the concatenated text content of a node.
func textContent(n *html.Node) string {
	var buf strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			buf.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(buf.String())
}

// extractEntryName extracts a file or directory name from the last path segment of an href.
func extractEntryName(href string) string {
	href = strings.TrimSuffix(href, "/")
	idx := strings.LastIndex(href, "/")
	segment := href
	if idx >= 0 {
		segment = href[idx+1:]
	}
	if segment == "" || segment == ".." {
		return ""
	}
	return segment
}

// parseGCSWebDirs reads GCSweb HTML and extracts directory names from <a> hrefs.
// It accepts any non-empty directory name (href ending with "/") and skips the
// ".." back link by checking both the href and the anchor text content.
func parseGCSWebDirs(r io.Reader) ([]string, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, fmt.Errorf("parsing HTML: %w", err)
	}

	var dirs []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			// Skip if the visible text of this link is ".." (back link).
			if anchorTextIsBackLink(n) {
				goto children
			}
			for _, attr := range n.Attr {
				if attr.Key == "href" {
					if name, ok := extractDirName(attr.Val); ok {
						dirs = append(dirs, name)
					}
				}
			}
		}
	children:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	return dirs, nil
}

// anchorTextIsBackLink returns true if the visible text content of an <a>
// element is ".." (the GCSweb parent-directory link).
func anchorTextIsBackLink(n *html.Node) bool {
	var buf strings.Builder
	var collect func(*html.Node)
	collect = func(n *html.Node) {
		if n.Type == html.TextNode {
			buf.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			collect(c)
		}
	}
	collect(n)
	return strings.TrimSpace(buf.String()) == ".."
}

// extractDirName extracts a directory name from a GCSweb href. It returns the
// directory name (without trailing slash) and true if the href refers to a
// directory, or ("", false) for back links, files, or empty segments.
func extractDirName(href string) (string, bool) {
	if !strings.HasSuffix(href, "/") {
		return "", false
	}
	href = strings.TrimSuffix(href, "/")
	idx := strings.LastIndex(href, "/")
	segment := href
	if idx >= 0 {
		segment = href[idx+1:]
	}
	if segment == "" || segment == ".." {
		return "", false
	}
	return segment, true
}

// clusterFlavorRules maps test name keywords to cluster directory name fragments.
// The cluster dirs in GCS are named capz-e2e-{random}-{flavor}, where flavor
// corresponds to the prow CI template names from
// templates/test/ci/cluster-template-prow-{flavor}.yaml in the CAPZ repo.
var clusterFlavorRules = []struct {
	testKeywords    []string // any of these in the test name (lowercased)
	clusterKeywords []string // cluster dir must contain any of these
}{
	// Specific flavors first (more specific matches before general ones)
	{testKeywords: []string{"flatcar"}, clusterKeywords: []string{"flatcar-sysext"}},
	{testKeywords: []string{"azure linux 3", "azurelinux 3", "azl3"}, clusterKeywords: []string{"azl3"}},
	{testKeywords: []string{"azure cni v1", "cni v1", "cni-v1"}, clusterKeywords: []string{"azure-cni-v1"}},
	{testKeywords: []string{"rke2"}, clusterKeywords: []string{"rke2"}},
	{testKeywords: []string{"clusterclass", "cluster class"}, clusterKeywords: []string{"clusterclass", "topology"}},
	{testKeywords: []string{"edgezone", "edge zone"}, clusterKeywords: []string{"edgezone"}},
	{testKeywords: []string{"nvidia", "gpu"}, clusterKeywords: []string{"nvidia-gpu"}},
	{testKeywords: []string{"spot"}, clusterKeywords: []string{"spot"}},
	{testKeywords: []string{"private"}, clusterKeywords: []string{"private"}},
	{testKeywords: []string{"custom vnet", "custom-vnet"}, clusterKeywords: []string{"custom-vnet"}},
	{testKeywords: []string{"apiserver-ilb", "internal load balancer"}, clusterKeywords: []string{"apiserver-ilb"}},
	{testKeywords: []string{"aks"}, clusterKeywords: []string{"-aks"}},
	{testKeywords: []string{"machine pool", "machinepool", "vmss"}, clusterKeywords: []string{"machine-pool"}},
	{testKeywords: []string{"dual-stack", "dualstack", "dual stack"}, clusterKeywords: []string{"dual-stack"}},
	{testKeywords: []string{"ipv6"}, clusterKeywords: []string{"ipv6"}},
	{testKeywords: []string{"windows"}, clusterKeywords: []string{"windows"}},
	{testKeywords: []string{"ha ", "ha cluster", "highly available", "highly-available"}, clusterKeywords: []string{"-ha"}},
	{testKeywords: []string{"dalec"}, clusterKeywords: []string{"dalec"}},
	{testKeywords: []string{"ci version", "ci-version", "ci artifacts"}, clusterKeywords: []string{"ci-version"}},
}

// MapTestToCluster attempts to match a test name to a discovered cluster.
// If only one cluster was discovered, it always matches. Otherwise heuristics
// based on keywords in the test name are used, referencing the CAPZ prow
// CI template flavors.
func MapTestToCluster(testName string, clusters []models.ClusterArtifacts) *models.ClusterArtifacts {
	if len(clusters) == 0 {
		return nil
	}
	if len(clusters) == 1 {
		return &clusters[0]
	}

	lower := strings.ToLower(testName)

	// Find all rules that match the test name, in priority order (first = most specific).
	// For each matching rule, check if a corresponding cluster exists.
	// Return the first match. If the most specific rule has no cluster, return nil
	// rather than falling through to a less specific rule — this prevents e.g.
	// an "Azure Linux 3 HA" test from incorrectly matching the HA cluster when
	// the azl3 cluster doesn't exist (because the cluster wasn't created).
	type ruleMatch struct {
		ruleIdx int
		cluster *models.ClusterArtifacts
	}

	var matches []ruleMatch
	for ri, r := range clusterFlavorRules {
		testMatch := false
		for _, kw := range r.testKeywords {
			if strings.Contains(lower, kw) {
				testMatch = true
				break
			}
		}
		if !testMatch {
			continue
		}
		// Rule matches the test name. Check if any cluster matches.
		found := false
		for i := range clusters {
			clusterLower := strings.ToLower(clusters[i].ClusterName)
			for _, ckw := range r.clusterKeywords {
				if strings.Contains(clusterLower, ckw) {
					matches = append(matches, ruleMatch{ruleIdx: ri, cluster: &clusters[i]})
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			// Rule matched test name but no cluster exists — record a nil match
			// to block less-specific rules from being used.
			matches = append(matches, ruleMatch{ruleIdx: ri, cluster: nil})
		}
	}

	if len(matches) == 0 {
		return nil
	}

	// Return the highest-priority (earliest rule) match that has a cluster.
	// But if a more specific rule (earlier) had no cluster, don't fall through.
	// Only fall through to the next rule if it's equally specific (same priority tier).
	firstMatch := matches[0]
	if firstMatch.cluster != nil {
		return firstMatch.cluster
	}
	// Most specific rule had no cluster — return nil.
	return nil
}
