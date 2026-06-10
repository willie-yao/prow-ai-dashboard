// Package tools defines the agentic tool interface and registry used by the
// AI loop. Tools are stateless from the model's perspective: each call is
// context-bound server-side (via ToolEnv) so the agent never sees bucket,
// job, or build identifiers in tool schemas.
//
// The registry supports two ways to enable tools:
//
//	tools: [filesystem, k8s]          // enable whole groups
//	tools: [filesystem, k8s.discover_clusters]  // mix groups and individual tools
//
// Tier-1 tools (group "filesystem") give the model raw artifact-tree access.
// Tier-2 tools (group "k8s") encode Kubernetes-shape navigation primitives
// (cluster discovery, machine logs, controller logs) so the agent does not
// have to compose them from list/read/tail on every project.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
)

// Schema is the OpenAI-shape tool definition emitted in the tools array of a
// chat-completion request. Tools own their own schema so the registry can
// build the per-request slice without duplicating the description elsewhere.
type Schema struct {
	Type     string       `json:"type"`
	Function FunctionDecl `json:"function"`
}

// FunctionDecl is the function half of an OpenAI tool definition.
type FunctionDecl struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// Result is what a Tool returns from Dispatch. Payload is the inner JSON
// object the loop will hand back to the model in a "role: tool" message;
// the loop wraps it with an envelope containing remaining-budget fields so
// every tool's response looks the same to the model.
//
// BudgetExhausted is a typed signal so the agentic loop can stamp
// AIAnalysis.BudgetExhausted without string-matching error messages.
// BytesFetched is GCS bytes actually pulled by this call (added to the
// per-analysis GCS budget); zero when nothing was fetched (e.g. error,
// cache hit, listing-only call).
//
// A tool that wants to surface an error to the model uses ErrPayload as a
// shortcut; the loop will still apply the envelope.
type Result struct {
	Payload         map[string]interface{}
	BudgetExhausted bool
	BytesFetched    int
}

// ErrPayload returns a Result whose Payload contains a single "error" key.
func ErrPayload(msg string) Result {
	return Result{Payload: map[string]interface{}{"error": msg}}
}

// Tool is the unit of agent capability. Name() must be unique within the
// registry; Group() is the alias consumers can enable in bulk
// (e.g. "filesystem", "k8s"). Schema() is included in every chat request
// that exposes this tool; Dispatch is invoked once per model tool_call.
type Tool interface {
	Name() string
	Group() string
	Schema() Schema
	Dispatch(ctx context.Context, env *Env, args json.RawMessage) Result
}

// Env is the per-analysis context passed to every Tool. It deliberately
// does not expose the agent's loop state (iteration counter, model bytes,
// pending messages) so tools cannot mutate loop internals.
type Env struct {
	// Browser is the per-build artifact view. Always non-nil.
	Browser artifacts.Browser

	// Cache is a per-build memoization layer. Tools should use it to cache
	// expensive discovery results (cluster listings, controller-log
	// enumerations, etc.) so 50 failed tests in the same build do not pay
	// the same GCS cost 50 times.
	//
	// Keys are typed as "tool/args" (callers compose); values are
	// caller-defined (typically marshaled JSON). The cache is shared
	// across all failures of one build and is bounded by the host
	// process's memory rather than a hard cap (the entries are listings
	// + URL maps, kilobytes each, not log content).
	Cache *Cache

	// WebURLBase is the GCSweb-style base URL of the build root (e.g.
	// "https://gcsweb.k8s.io/gcs/<bucket>/logs/<job>/<build>/"). Used by
	// k8s tools to render web_url fields alongside path fields so the
	// frontend can keep linking to artifacts without ClusterArtifacts.
	// May be empty when the caller doesn't know the web URL; tools must
	// degrade gracefully (omit web_url, still return path).
	WebURLBase string

	// RemainingModelBytes / RemainingGCSBytes report budgets at dispatch
	// time. Tools that do heavy work should bail early when these are
	// near zero, returning Result{BudgetExhausted: true} so the loop can
	// finalize.
	RemainingModelBytes int
	RemainingGCSBytes   int
}

// Registry maps tool names to Tool implementations and tracks groups for
// bulk enablement. A Registry is constructed per agentic call (cheap) so
// per-failure config differences can be honored.
type Registry struct {
	tools  map[string]Tool
	groups map[string][]string // group → tool names
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		tools:  map[string]Tool{},
		groups: map[string][]string{},
	}
}

// Register adds a Tool. Duplicate names panic (programmer error).
func (r *Registry) Register(t Tool) {
	if _, dup := r.tools[t.Name()]; dup {
		panic("tools: duplicate tool name: " + t.Name())
	}
	r.tools[t.Name()] = t
	r.groups[t.Group()] = append(r.groups[t.Group()], t.Name())
}

// Enable resolves a config list like ["filesystem", "k8s.discover_clusters"]
// into a deduplicated set of tool names. Unknown entries error so a typo in
// project.yaml fails loudly. The returned slice is sorted for determinism.
func (r *Registry) Enable(entries []string) ([]string, error) {
	enabled := map[string]struct{}{}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		// Individual tool name (always contains "."): "k8s.discover_clusters"
		if strings.Contains(e, ".") {
			_, name, _ := strings.Cut(e, ".")
			if _, ok := r.tools[name]; !ok {
				return nil, fmt.Errorf("unknown tool: %q", e)
			}
			enabled[name] = struct{}{}
			continue
		}
		// Group alias: "filesystem"
		names, ok := r.groups[e]
		if !ok {
			return nil, fmt.Errorf("unknown tool or group: %q", e)
		}
		for _, n := range names {
			enabled[n] = struct{}{}
		}
	}
	out := make([]string, 0, len(enabled))
	for n := range enabled {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// Schemas returns the OpenAI tool definitions for the given enabled names,
// sorted by name for determinism (so equivalent configs produce equivalent
// system prompts and cache keys).
func (r *Registry) Schemas(enabled []string) []Schema {
	out := make([]Schema, 0, len(enabled))
	for _, n := range enabled {
		t, ok := r.tools[n]
		if !ok {
			continue
		}
		out = append(out, t.Schema())
	}
	return out
}

// Dispatch invokes the named tool with the given JSON arguments. Returns a
// Result with an error payload if the tool is not in the registry. The
// caller is responsible for adding the result to the message list and for
// honoring Result.BudgetExhausted.
func (r *Registry) Dispatch(ctx context.Context, env *Env, name string, args json.RawMessage) Result {
	t, ok := r.tools[name]
	if !ok {
		return ErrPayload("unknown tool: " + name)
	}
	return t.Dispatch(ctx, env, args)
}
