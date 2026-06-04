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
// Module-private input to buildAnalysisPrompt.
type evidence struct {
	TestName         string
	FailureMessage   string
	FailureBody      string
	ClusterFlavor    string
	ConsecutiveCount int

	BuildLogErrors string
	BuildLogTail   string
	ResourceYAMLs  map[string]string

	// MachineLogs maps a declared filename (e.g. "kubelet.log") to the
	// tail of that log from the first machine that had a URL for it.
	MachineLogs map[string]string

	// ControllerLogs maps "<namespace>/<deployment>" to the tail of the
	// matched controller pod's container_log.
	ControllerLogs map[string]string

	// ProviderActivityLog is the cloud-provider audit log (e.g. Azure
	// activity log) sourced from cluster artifacts.
	ProviderActivityLog string

	// RequestedButMissing lists evidence sources the project config asked
	// for that produced no content. Surfaced in the prompt footer so the
	// AI doesn't speculate based on absence.
	RequestedButMissing []string
}

var (
	// errorRe matches "error" lines; noErrorRe filters "no error".
	errorRe = regexp.MustCompile(`(?i)error`)
	// connectionRefusedRe is included only when ≥5 occurrences in the
	// build log indicate a persistent issue worth surfacing.
	connectionRefusedRe = regexp.MustCompile(`(?i)connection refused`)
)

// Per-entry and aggregate caps for controller log inclusion in the prompt.
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

// collectControllerLogs fetches controller log tails from URLs the collector
// recorded on BuildResult.ControllerLogURLs (keyed by "<namespace>/<deployment>"
// since a namespace can host multiple controller deployments). Aggregate size
// is capped by totalControllerLogCap; each fetch by perControllerLogCap.
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

// extractHrefs parses a GCSweb HTML listing and returns href values matching
// the given predicate. Shared by directory and file-extension listings.
func extractHrefs(r io.Reader, match func(href string) bool) ([]string, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, err
	}

	var hits []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, attr := range n.Attr {
				if attr.Key == "href" && match(attr.Val) {
					hits = append(hits, attr.Val)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return hits, nil
}

// parseResourceDirs extracts directory names from a GCSweb HTML listing,
// filtering out the ".." back link.
func parseResourceDirs(r io.Reader) ([]string, error) {
	hrefs, err := extractHrefs(r, func(h string) bool { return strings.HasSuffix(h, "/") })
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, href := range hrefs {
		name := strings.TrimSuffix(href, "/")
		if idx := strings.LastIndex(name, "/"); idx >= 0 {
			name = name[idx+1:]
		}
		if name != "" && name != ".." {
			dirs = append(dirs, name)
		}
	}
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
	return extractHrefs(r, func(h string) bool { return strings.HasSuffix(h, ".yaml") })
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
