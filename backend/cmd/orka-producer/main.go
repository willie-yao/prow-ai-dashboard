// Command orka-producer turns a fetcher-produced dashboard skeleton into Orka
// analysis Tasks: one content-addressed `type: ai` Task per failing test. It
// reuses the engine's discovery output (jobs/*.json) and prompt composition, so
// the only thing that moves off the engine is the analysis step itself.
//
// Build isolation (model-independent): for each distinct build, the producer
// clones the base Tool CRDs with a static `X-Build-Prefix` header and a
// build-suffixed name, and each Task references its build's tool set. The shim
// routes by that header, so tools always read the Task's own build regardless of
// what the model does. (A model-passed `build` param proved unreliable.)
//
// Run-once: each Task name is content-addressed (az-<build>-<failureHash>-<ver>),
// so re-running and re-applying is idempotent. Bump -version to force re-analysis
// after a prompt/tool change.
//
// The producer is pure: it writes Task + Tool YAMLs to -tasks-out / -tools-out.
// Applying them (kubectl apply -f) is the orchestration step's job.
//
// TEMPORARY: lives only on the `orka` branch alongside experimental/orka/.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orkamig"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

var defaultTools = []string{
	"list-artifacts", "find-artifacts", "grep-artifact", "read-artifact", "tail-artifact",
	"discover-clusters", "find-my-cluster", "list-cluster-machines", "list-machine-logs",
	"discover-controllers", "resolve-controller-log",
	"validate-analysis", "verify-timeline", "check-transient-signatures", "recurrence", "required-evidence",
}

func main() {
	dataDir := flag.String("data", "data", "dashboard skeleton dir (fetcher output, holds jobs/*.json)")
	projectDir := flag.String("project-dir", ".", "consumer dir with project.yaml + prompts/system.md")
	toolManifests := flag.String("tool-manifests", "experimental/orka/manifests", "dir with base Tool CRD YAMLs to clone per build")
	tasksOut := flag.String("tasks-out", "tasks", "output dir for generated Task YAMLs")
	toolsOut := flag.String("tools-out", "build-tools", "output dir for generated per-build Tool YAMLs")
	namespace := flag.String("namespace", "orka-system", "namespace for the Tasks and Tools")
	provider := flag.String("provider", "copilot", "Orka Provider name")
	model := flag.String("model", "claude-sonnet-4.5", "model id")
	version := flag.String("version", "v1", "content-address version suffix; bump to force re-analysis")
	timeout := flag.String("timeout", "10m", "per-Task timeout")
	toolsCSV := flag.String("tools", strings.Join(defaultTools, ","), "comma-separated base Tool names to enable")
	flag.Parse()

	_, addendum, err := project.LoadDir(*projectDir)
	if err != nil {
		log.Fatalf("load project %s: %v", *projectDir, err)
	}
	systemPrompt := ai.ComposeSystemPrompt(addendum) + toolUsageAddendum

	toolNames := splitCSV(*toolsCSV)
	baseTools, err := loadBaseTools(*toolManifests, toolNames)
	if err != nil {
		log.Fatalf("load base tools: %v", err)
	}

	mustMkdir(*tasksOut)
	mustMkdir(*toolsOut)

	jobFiles, _ := filepath.Glob(filepath.Join(*dataDir, "jobs", "*.json"))
	builds := map[string]string{} // buildID -> buildPrefix (distinct builds seen)
	tasks := 0
	for _, jf := range jobFiles {
		var detail models.JobDetail
		if b, err := os.ReadFile(jf); err != nil || json.Unmarshal(b, &detail) != nil {
			continue
		}
		for _, run := range detail.Runs {
			buildPrefix := fmt.Sprintf("logs/%s/%s/", detail.JobID, run.BuildID)
			for _, tc := range run.TestCases {
				if tc.Status != "failed" {
					continue
				}
				builds[run.BuildID] = buildPrefix
				name := orkamig.TaskName(run.BuildID, orkamig.FailureHash(tc.Name, tc.FailureMessage), *version)
				task := buildTask(name, *namespace, *provider, *model, *timeout,
					buildToolNames(toolNames, run.BuildID), systemPrompt,
					userPrompt(detail.JobID, buildPrefix, tc))
				writeYAML(filepath.Join(*tasksOut, name+".yaml"), task)
				tasks++
			}
		}
	}

	// Emit per-build Tool CRD clones (header-routed) for every distinct build.
	toolDocs := 0
	for buildID, prefix := range builds {
		for _, base := range toolNames {
			doc := baseTools[base]
			clone := cloneToolForBuild(doc, base, buildID, prefix, *namespace)
			writeYAML(filepath.Join(*toolsOut, buildToolName(base, buildID)+".yaml"), clone)
			toolDocs++
		}
	}

	log.Printf("wrote %d Tasks (%s) and %d per-build Tools across %d builds (%s)",
		tasks, *tasksOut, toolDocs, len(builds), *toolsOut)
}

const toolUsageAddendum = `

## Tool usage for this analysis
The tools are scoped to THIS task's build automatically; just call them normally.
Resolve the right per-spec cluster (find_my_cluster) before reading per-cluster
logs. For a transient-vs-bug decision, confirm any transient claim with
verify_timeline (did the expected operation actually register?) and
check_transient_signatures, and consult recurrence. Default to is_transient=false
unless a known transient class is proven from the evidence. Call validate_analysis
on every artifact path you cite.

## Self-critique before you finalize
Before emitting your JSON, re-check your own draft for these specific defects and
revise if any applies:
1. Causal ordering: is your root_cause the EARLIEST initiating failure, or a later
   downstream/teardown symptom (namespace/cluster deletion, cleanup timeout,
   credential expiry, a cascade of dependent timeouts)? Those happen AFTER the
   real failure.
2. Attribution: if you blamed an external/platform cause (throttling, a hung Azure
   operation, upstream flakiness) and set is_transient=true, did verify_timeline
   actually show the expected operation never registered? If you did not confirm
   it, treat the failure as a real bug (is_transient=false).
3. Grounding: is every claim tied to evidence you actually read (validate_analysis
   passed), not plausible-sounding speculation?
4. Fix validity: would suggested_fix actually resolve the stated root_cause?
Respond with ONLY the required JSON object.`

func userPrompt(jobID, buildPrefix string, tc models.TestCase) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This CAPZ CI test FAILED. Root-cause it and classify transient vs real bug.\n\n")
	fmt.Fprintf(&b, "Job: %s\n", jobID)
	fmt.Fprintf(&b, "Build: %s\n", buildPrefix)
	fmt.Fprintf(&b, "Failed test: %s\n", tc.Name)
	if tc.FailureLocation != "" {
		fmt.Fprintf(&b, "Failure location: %s\n", tc.FailureLocation)
	}
	if tc.JUnitFile != "" {
		fmt.Fprintf(&b, "JUnit file: %s\n", tc.JUnitFile)
	}
	msg := tc.FailureMessage
	if len(msg) > 1200 {
		msg = msg[:1200]
	}
	if msg != "" {
		fmt.Fprintf(&b, "Failure output:\n%s\n", msg)
	}
	fmt.Fprintf(&b, "\nInvestigate the build's artifacts with the tools and conclude with your JSON.")
	return b.String()
}

func buildTask(name, namespace, provider, model, timeout string, tools []string, systemPrompt, prompt string) map[string]any {
	return map[string]any{
		"apiVersion": "core.orka.ai/v1alpha1",
		"kind":       "Task",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    map[string]any{"app.kubernetes.io/managed-by": "orka-producer"},
		},
		"spec": map[string]any{
			"type":    "ai",
			"timeout": timeout,
			"ai": map[string]any{
				"providerRef":  map[string]any{"name": provider},
				"model":        model,
				"tools":        tools,
				"systemPrompt": systemPrompt,
				"prompt":       prompt,
			},
		},
	}
}

// loadBaseTools parses Tool CRDs from every YAML doc under dir and returns the
// ones named in want, keyed by name.
func loadBaseTools(dir string, want []string) (map[string]map[string]any, error) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	out := map[string]map[string]any{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var doc map[string]any
			if err := dec.Decode(&doc); err != nil {
				break
			}
			if doc == nil || asString(doc["kind"]) != "Tool" {
				continue
			}
			meta, _ := doc["metadata"].(map[string]any)
			out[asString(meta["name"])] = doc
		}
	}
	for _, w := range want {
		if _, ok := out[w]; !ok {
			return nil, fmt.Errorf("base Tool %q not found under %s", w, dir)
		}
	}
	return out, nil
}

// cloneToolForBuild copies a base Tool CRD, renames it per build, and injects the
// X-Build-Prefix header so the shim serves this build.
func cloneToolForBuild(base map[string]any, baseName, buildID, prefix, namespace string) map[string]any {
	doc := deepCopy(base).(map[string]any)
	meta, _ := doc["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		doc["metadata"] = meta
	}
	meta["name"] = buildToolName(baseName, buildID)
	meta["namespace"] = namespace
	meta["labels"] = map[string]any{"app.kubernetes.io/managed-by": "orka-producer"}

	spec, _ := doc["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
		doc["spec"] = spec
	}
	httpCfg, _ := spec["http"].(map[string]any)
	if httpCfg == nil {
		httpCfg = map[string]any{}
		spec["http"] = httpCfg
	}
	headers, _ := httpCfg["headers"].(map[string]any)
	if headers == nil {
		headers = map[string]any{}
		httpCfg["headers"] = headers
	}
	headers["X-Build-Prefix"] = prefix
	return doc
}

func buildToolNames(base []string, buildID string) []string {
	out := make([]string, len(base))
	for i, b := range base {
		out[i] = buildToolName(b, buildID)
	}
	return out
}

func buildToolName(base, buildID string) string { return orkamig.Sanitize(base + "-b" + buildID) }

// --- helpers ---

func writeYAML(path string, v any) {
	out, err := yaml.Marshal(v)
	if err != nil {
		log.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		log.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdir(dir string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", dir, err)
	}
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k := range t {
			m[k] = deepCopy(t[k])
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i := range t {
			s[i] = deepCopy(t[i])
		}
		return s
	default:
		return v
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
