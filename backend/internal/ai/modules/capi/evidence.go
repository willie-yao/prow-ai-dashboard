package capi

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/net/html"

	evidencepkg "github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/evidence"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/gcs"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// evidence holds all artifact content gathered for a single test failure.
// Module-private: only used to feed buildAnalysisPrompt.
//
// MachineLogs and ControllerLogs are keyed by the consumer-declared identifier
// (filename for machine logs, namespace for controller logs) so the prompt
// renders one section per request and stays in sync with the project config.
type evidence struct {
	TestName         string
	FailureMessage   string
	FailureBody      string
	ClusterFlavor    string
	ConsecutiveCount int

	BuildLogErrors string
	BuildLogTail   string
	ResourceYAMLs  map[string]string

	// MachineLogs maps a declared filename (e.g. "kubelet.log") to the tail
	// of that log fetched from the first machine that had a non-empty URL
	// for the file.
	MachineLogs map[string]string

	// ControllerLogs maps a declared namespace (e.g. "capi-system") to the
	// tail of the matched controller pod's container_log.
	ControllerLogs map[string]string

	// ProviderActivityLog is the cloud-provider audit log (e.g. Azure
	// activity log) sourced from the cluster artifacts directly, not from
	// the evidence config.
	ProviderActivityLog string

	// RequestedButMissing lists evidence sources the project config asked
	// for that produced no content (e.g. boot.log on a CAPD build with no
	// VMs, or capz-system controller logs on a CAPI core build). Surfaced
	// in the prompt footer so the AI doesn't speculate based on absence.
	RequestedButMissing []string
}

// errorRe matches "error" lines; "no error" lines are filtered out via noErrorRe.
var errorRe = regexp.MustCompile(`(?i)error`)

// connectionRefusedRe is included only when ≥5 occurrences in the build log
// indicate a persistent issue worth surfacing to the AI.
var connectionRefusedRe = regexp.MustCompile(`(?i)connection refused`)

// Budget for controller logs: each entry caps at perControllerLogCap bytes
// and the aggregate across all controller logs caps at totalControllerLogCap.
// Keeps the prompt size bounded when a build has many CAPI namespaces.
const (
	perControllerLogCap   = 8000
	totalControllerLogCap = 50000
)

// collectEvidence gathers all available artifact content for a test failure.
// Errors fetching individual artifacts are logged but do not fail the overall
// collection — the prompt is built from whatever is available.
func (m *Module) collectEvidence(ctx context.Context, client *http.Client, run *models.BuildResult, tc *models.TestCase, consecutive int) evidence {
	ev := evidence{
		TestName:         tc.Name,
		FailureMessage:   tc.FailureMessage,
		FailureBody:      tc.FailureBody,
		ClusterFlavor:    m.flavor(tc),
		ConsecutiveCount: consecutive,
		MachineLogs:      map[string]string{},
		ControllerLogs:   map[string]string{},
	}

	// 1. Build log: pattern-matched error lines + raw tail.
	if run.BuildLogURL != "" {
		ev.BuildLogErrors = collectBuildLogErrors(ctx, client, run.BuildLogURL, m.evidence.BuildLogPatterns)
		ev.BuildLogTail = evidencepkg.FetchLogTail(ctx, client, run.BuildLogURL, 500, 15000)
	}

	// 2. Bootstrap resource YAMLs (status: blocks from every resource type).
	if tc.ClusterArtifacts != nil && tc.ClusterArtifacts.BootstrapResourcesURL != "" {
		ev.ResourceYAMLs = collectAllResources(ctx, client, tc.ClusterArtifacts.BootstrapResourcesURL)
	}

	// 3. Per-machine logs declared by the consumer. For each filename, pick
	// the first machine that has a non-empty URL for it. Filenames the
	// consumer asked for but no machine has are recorded as missing so the
	// AI doesn't reach a conclusion from absence.
	collectMachineLogs(ctx, client, tc.ClusterArtifacts, m.evidence.MachineLogs, &ev)

	// 4. Controller logs declared by the consumer. Iterate selectors, fetch
	// the matching pod's container_log URL from BuildResult.ControllerLogURLs
	// (populated by the collector), respect the total budget.
	collectControllerLogs(ctx, client, run, m.evidence.ControllerLogs, &ev)

	// 5. Provider activity log (cloud audit log; CAPZ-only today). Comes
	// from cluster_artifacts, not the evidence config.
	if tc.ClusterArtifacts != nil && tc.ClusterArtifacts.ProviderActivityLog != "" {
		ev.ProviderActivityLog = collectActivityLog(ctx, client, tc.ClusterArtifacts.ProviderActivityLog)
	}

	return ev
}

// collectMachineLogs walks the declared machine_logs and populates ev.MachineLogs
// for each filename it could fetch. Filenames that produced no content are
// appended to ev.RequestedButMissing.
func collectMachineLogs(ctx context.Context, client *http.Client, ca *models.ClusterArtifacts, declared []string, ev *evidence) {
	if len(declared) == 0 {
		return
	}
	for _, filename := range declared {
		if ca == nil || len(ca.Machines) == 0 {
			ev.RequestedButMissing = append(ev.RequestedButMissing, "machine log "+filename)
			continue
		}
		var content string
		for _, machine := range ca.Machines {
			url, ok := machine.Logs[filename]
			if !ok || url == "" {
				continue
			}
			content = evidencepkg.FetchLogTail(ctx, client, url, 500, 15000)
			if content != "" {
				break
			}
		}
		if content == "" {
			ev.RequestedButMissing = append(ev.RequestedButMissing, "machine log "+filename)
			continue
		}
		ev.MachineLogs[filename] = content
	}
}

// collectControllerLogs walks the declared controller_logs selectors and
// fetches the matching controller pod's container_log tail from the URLs the
// collector recorded on BuildResult.ControllerLogURLs. The collector keys
// URLs by "<namespace>/<deployment>" (a single namespace can host multiple
// controller deployments — e.g. capz-system runs both ASO and the CAPZ
// controller — and we want all of them in the prompt). Aggregate size capped
// at totalControllerLogCap; each fetch capped at perControllerLogCap.
func collectControllerLogs(ctx context.Context, client *http.Client, run *models.BuildResult, declared []project.ControllerLogSelector, ev *evidence) {
	if len(declared) == 0 {
		return
	}
	var totalBytes int
	for _, sel := range declared {
		// Find every URL whose namespace prefix matches this selector.
		// Sort for determinism across runs.
		var keys []string
		for k := range run.ControllerLogURLs {
			ns, _, _ := strings.Cut(k, "/")
			if ns == sel.Namespace {
				keys = append(keys, k)
			}
		}
		if len(keys) == 0 {
			ev.RequestedButMissing = append(ev.RequestedButMissing, "controller log "+sel.Namespace)
			continue
		}
		sort.Strings(keys)

		for _, key := range keys {
			if totalBytes >= totalControllerLogCap {
				ev.RequestedButMissing = append(ev.RequestedButMissing, "controller log "+key+" (budget exhausted)")
				continue
			}
			url := run.ControllerLogURLs[key]
			remaining := totalControllerLogCap - totalBytes
			cap := perControllerLogCap
			if remaining < cap {
				cap = remaining
			}
			content := evidencepkg.FetchLogTail(ctx, client, url, 500, cap)
			if content == "" {
				ev.RequestedButMissing = append(ev.RequestedButMissing, "controller log "+key)
				continue
			}
			ev.ControllerLogs[key] = content
			totalBytes += len(content)
		}
	}
}

// collectBuildLogErrors fetches the build log and returns matching lines from
// the configured patterns plus 2 lines of context around each match.
func collectBuildLogErrors(ctx context.Context, client *http.Client, url string, patterns []*regexp.Regexp) string {
	data, err := gcs.FetchRaw(ctx, client, url)
	if err != nil {
		log.Printf("  ⚠ Evidence: failed to fetch build log: %v", err)
		return ""
	}

	lines := strings.Split(string(data), "\n")

	// Count connection refused occurrences to detect persistent issues.
	connRefusedCount := 0
	for _, line := range lines {
		if connectionRefusedRe.MatchString(line) {
			connRefusedCount++
		}
	}
	includeConnRefused := connRefusedCount >= 5

	matchSet := make(map[int]bool)
	noErrorRe := regexp.MustCompile(`(?i)no error`)
	for i, line := range lines {
		for _, pat := range patterns {
			if pat.MatchString(line) {
				matchSet[i] = true
				break
			}
		}
		// Always include bare "error" (but not "no error") because the raw
		// build-log tail is also in the prompt for context — this gives the
		// AI both error-grep and trailing-context views.
		if !matchSet[i] && errorRe.MatchString(line) && !noErrorRe.MatchString(line) {
			matchSet[i] = true
		}
		if !matchSet[i] && includeConnRefused && connectionRefusedRe.MatchString(line) {
			matchSet[i] = true
		}
	}

	contextSet := make(map[int]bool)
	for idx := range matchSet {
		for c := idx - 2; c <= idx+2; c++ {
			if c >= 0 && c < len(lines) {
				contextSet[c] = true
			}
		}
	}

	var sb strings.Builder
	prevIdx := -10
	for i := 0; i < len(lines); i++ {
		if !contextSet[i] {
			continue
		}
		if i > prevIdx+1 && sb.Len() > 0 {
			sb.WriteString("---\n")
		}
		sb.WriteString(lines[i])
		sb.WriteByte('\n')
		prevIdx = i

		if sb.Len() > 15000 {
			break
		}
	}

	result := sb.String()
	if len(result) > 15000 {
		result = result[:15000] + "..."
	}
	return result
}

// collectAllResources discovers all resource type directories in the bootstrap
// resources namespace and fetches status YAMLs from each.
func collectAllResources(ctx context.Context, client *http.Client, baseURL string) map[string]string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("  ⚠ Evidence: failed to list resource types at %s: %v", baseURL, err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	dirs, err := parseResourceDirs(resp.Body)
	if err != nil {
		return nil
	}

	results := make(map[string]string)
	totalChars := 0
	const maxTotalChars = 60000

	for _, dir := range dirs {
		if totalChars > maxTotalChars {
			break
		}
		remaining := maxTotalChars - totalChars
		if remaining < 1000 {
			break
		}
		maxPerType := 8000
		if remaining < maxPerType {
			maxPerType = remaining
		}

		content := collectResourceStatus(ctx, client, baseURL+dir+"/", maxPerType)
		if content != "" {
			results[dir] = content
			totalChars += len(content)
		}
	}

	return results
}

// parseResourceDirs extracts directory names from a GCSweb HTML listing,
// filtering out the ".." back link.
func parseResourceDirs(r io.Reader) ([]string, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, err
	}

	var dirs []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, attr := range n.Attr {
				if attr.Key == "href" && strings.HasSuffix(attr.Val, "/") {
					name := attr.Val
					name = strings.TrimSuffix(name, "/")
					if idx := strings.LastIndex(name, "/"); idx >= 0 {
						name = name[idx+1:]
					}
					if name != "" && name != ".." {
						dirs = append(dirs, name)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return dirs, nil
}

// collectResourceStatus fetches a GCSweb resource directory listing, downloads each
// YAML file, and extracts the status: section from each.
func collectResourceStatus(ctx context.Context, client *http.Client, listingURL string, maxChars int) string {
	yamlURLs, err := fetchYAMLFileLinks(ctx, client, listingURL)
	if err != nil {
		log.Printf("  ⚠ Evidence: failed to list resources at %s: %v", listingURL, err)
		return ""
	}

	var sb strings.Builder
	for _, url := range yamlURLs {
		data, err := gcs.FetchRaw(ctx, client, url)
		if err != nil {
			log.Printf("  ⚠ Evidence: failed to fetch resource YAML %s: %v", url, err)
			continue
		}

		status := extractYAMLStatus(string(data))
		if status == "" {
			continue
		}

		name := url
		if idx := strings.LastIndex(url, "/"); idx >= 0 {
			name = url[idx+1:]
		}
		fmt.Fprintf(&sb, "--- %s ---\n%s\n", name, status)

		if sb.Len() > maxChars {
			break
		}
	}

	result := sb.String()
	if len(result) > maxChars {
		result = result[:maxChars] + "..."
	}
	return result
}

// fetchYAMLFileLinks fetches a GCSweb listing page and returns URLs to .yaml files.
func fetchYAMLFileLinks(ctx context.Context, client *http.Client, listingURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listingURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", listingURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d for %s", resp.StatusCode, listingURL)
	}

	return parseYAMLLinks(resp.Body)
}

// parseYAMLLinks parses GCSweb HTML and extracts href values ending in .yaml.
func parseYAMLLinks(r io.Reader) ([]string, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, fmt.Errorf("parsing HTML: %w", err)
	}

	var urls []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, attr := range n.Attr {
				if attr.Key == "href" && strings.HasSuffix(attr.Val, ".yaml") {
					urls = append(urls, attr.Val)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	return urls, nil
}

// extractYAMLStatus extracts the status: section from a Kubernetes resource YAML.
// It returns everything from the first top-level "status:" line to the end of the
// indented block (or end of file).
func extractYAMLStatus(yamlContent string) string {
	lines := strings.Split(yamlContent, "\n")
	var sb strings.Builder
	inStatus := false

	for _, line := range lines {
		if !inStatus {
			if strings.HasPrefix(line, "status:") {
				inStatus = true
				sb.WriteString(line)
				sb.WriteByte('\n')
			}
			continue
		}
		if line == "" {
			sb.WriteByte('\n')
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			sb.WriteString(line)
			sb.WriteByte('\n')
		} else {
			break
		}
	}

	return strings.TrimSpace(sb.String())
}

// collectActivityLog fetches the provider (Azure) activity log tail.
func collectActivityLog(ctx context.Context, client *http.Client, url string) string {
	return evidencepkg.FetchLogTail(ctx, client, url, 400, 10000)
}

// sortedKeys returns map keys sorted alphabetically. Used to render
// MachineLogs/ControllerLogs/ResourceYAMLs deterministically in the prompt.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
