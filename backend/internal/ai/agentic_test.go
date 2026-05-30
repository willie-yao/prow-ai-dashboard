package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/filesystem"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
)

// ---------- Test scaffolding ----------

// newTestRegistry returns a tools.Registry preloaded with the filesystem
// tier so tests exercise the real dispatch path. K8s tools are intentionally
// omitted: the in-memory fakeBrowser doesn't model GCS layout deeply enough
// for cluster discovery, and the filesystem tier is sufficient to validate
// the agentic loop end-to-end.
func newTestRegistry(t *testing.T) (*tools.Registry, []string) {
	t.Helper()
	r := tools.NewRegistry()
	filesystem.Register(r)
	enabled, err := r.Enable([]string{"filesystem"})
	if err != nil {
		t.Fatalf("registry.Enable: %v", err)
	}
	return r, enabled
}

// newTestAgenticInputs bundles the per-call agentic context for tests. Keeps
// the test bodies readable without repeating the boilerplate.
func newTestAgenticInputs(t *testing.T, browser artifacts.Browser, opts AgenticOptions) AgenticInputs {
	t.Helper()
	registry, enabled := newTestRegistry(t)
	return AgenticInputs{
		Browser:      browser,
		Opts:         opts,
		Registry:     registry,
		EnabledTools: enabled,
		Cache:        tools.NewCache(),
	}
}

// fakeBrowser is an in-memory artifacts.Browser for agentic tests.
type fakeBrowser struct {
	files map[string][]byte
	dirs  map[string][]string
}

func (b *fakeBrowser) BuildRoot() string { return "fake/build/1" }

func (b *fakeBrowser) List(_ context.Context, dir string) (*artifacts.Listing, error) {
	dir = strings.TrimSuffix(dir, "/")
	res := &artifacts.Listing{Dir: dir}
	if d, ok := b.dirs[dir]; ok {
		res.Dirs = d
	}
	prefix := dir + "/"
	if dir == "" {
		prefix = ""
	}
	for name, data := range b.files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		if strings.Contains(rest, "/") {
			continue
		}
		res.Files = append(res.Files, artifacts.FileInfo{Name: rest, Size: int64(len(data))})
	}
	return res, nil
}

func (b *fakeBrowser) Read(_ context.Context, p string, offset, length int) ([]byte, int64, error) {
	data, ok := b.files[p]
	if !ok {
		return nil, 0, fmt.Errorf("not found: %s", p)
	}
	if offset > len(data) {
		return nil, int64(len(data)), nil
	}
	end := offset + length
	if end > len(data) {
		end = len(data)
	}
	return data[offset:end], int64(len(data)), nil
}

func (b *fakeBrowser) Tail(_ context.Context, p string, lines, _ int) (*artifacts.TailResult, error) {
	data, ok := b.files[p]
	if !ok {
		return nil, fmt.Errorf("not found: %s", p)
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	content := strings.Join(all, "\n")
	return &artifacts.TailResult{
		FileSize:      int64(len(data)),
		LinesReturned: len(all),
		Content:       []byte(content),
	}, nil
}

func (b *fakeBrowser) Grep(_ context.Context, p string, re *regexp.Regexp, _, maxMatches, _ int) (*artifacts.GrepResult, error) {
	data, ok := b.files[p]
	if !ok {
		return nil, fmt.Errorf("not found: %s", p)
	}
	var matches []artifacts.GrepMatch
	for i, line := range strings.Split(string(data), "\n") {
		if re.MatchString(line) {
			matches = append(matches, artifacts.GrepMatch{LineNo: i + 1, Context: []string{fmt.Sprintf("> %d: %s", i+1, line)}})
		}
	}
	total := len(matches)
	if maxMatches > 0 && len(matches) > maxMatches {
		matches = matches[:maxMatches]
	}
	return &artifacts.GrepResult{
		FileSize:     int64(len(data)),
		TotalMatches: total,
		Matches:      matches,
		Truncated:    total > len(matches),
		BytesScanned: int64(len(data)),
	}, nil
}

// scriptedChatServer returns an httptest.Server that responds with a queue of
// pre-canned responses. Each request pops one response. The handler can be
// reprogrammed by direct assignment to handler if a test needs custom
// per-call logic (e.g. error responses).
type scriptedChatServer struct {
	*httptest.Server
	mu        sync.Mutex
	responses []string
	statuses  []int
	calls     int32
}

func newScriptedChatServer(t *testing.T) *scriptedChatServer {
	t.Helper()
	s := &scriptedChatServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.calls, 1)
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.responses) == 0 {
			http.Error(w, "no scripted response", http.StatusInternalServerError)
			return
		}
		body := s.responses[0]
		s.responses = s.responses[1:]
		status := http.StatusOK
		if len(s.statuses) > 0 {
			status = s.statuses[0]
			s.statuses = s.statuses[1:]
		}
		// Drain request body so the client doesn't hang.
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *scriptedChatServer) push(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses = append(s.responses, body)
	s.statuses = append(s.statuses, status)
}

// chatRespFinal builds a JSON chat-completion response with no tool calls.
func chatRespFinal(content string) string {
	c, _ := json.Marshal(content)
	return fmt.Sprintf(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%s}}]}`, c)
}

// chatRespToolCall builds a chat-completion response that invokes one tool.
func chatRespToolCall(id, name string, args map[string]interface{}) string {
	a, _ := json.Marshal(args)
	aStr, _ := json.Marshal(string(a))
	return fmt.Sprintf(
		`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]}}]}`,
		id, name, aStr,
	)
}

func newAgenticTestClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	c := NewClientWithOptions(Options{
		Token:    "test-token",
		CacheDir: t.TempDir(),
		Endpoint: serverURL,
		Model:    "claude-test",
	})
	return c
}

// shrinkCallDelay temporarily reduces callDelay for the duration of a test
// so agentic tests with multiple iterations don't add seconds of latency.
func shrinkCallDelay(t *testing.T) {
	t.Helper()
	old := callDelay
	callDelay = 1 * time.Millisecond
	t.Cleanup(func() { callDelay = old })
}

// ---------- Tests ----------

func TestAgentic_HappyPath_ToolThenFinalJSON(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Round 1: model calls list_artifacts.
	srv.push(200, chatRespToolCall("call_1", "list_artifacts", map[string]interface{}{"path": ""}))
	// Round 2: model returns final JSON.
	final := `{"summary":"DNS lookup failed","is_transient":false,"root_cause":"resolver pointed at stale nameserver","severity":"High","suggested_fix":"Update /etc/resolv.conf","relevant_files":["build-log.txt"]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{
		files: map[string][]byte{"build-log.txt": []byte("hello\nworld\n")},
		dirs:  map[string][]string{"": {"artifacts"}},
	}
	opts := AgenticOptions{MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second}

	summary, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:job:1:abc", "system", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if summary.Summary != "DNS lookup failed" {
		t.Errorf("summary mismatch: %q", summary.Summary)
	}
	if analysis.Mode != AgenticMode {
		t.Errorf("mode = %q, want %q", analysis.Mode, AgenticMode)
	}
	if analysis.RootCause != "resolver pointed at stale nameserver" {
		t.Errorf("root cause mismatch: %q", analysis.RootCause)
	}
	if atomic.LoadInt32(&srv.calls) != 2 {
		t.Errorf("call count = %d, want 2", srv.calls)
	}
	// Telemetry: one tool call (list_artifacts), non-zero modelBytes
	// (tool result echoed back to the model), elapsed > 0, cache_hit false.
	if analysis.ToolCalls != 1 {
		t.Errorf("tool_calls = %d, want 1", analysis.ToolCalls)
	}
	if analysis.ModelBytes <= 0 {
		t.Errorf("expected positive model_bytes, got %d", analysis.ModelBytes)
	}
	if analysis.CacheHit {
		t.Error("expected cache_hit=false on first call")
	}
	if analysis.ElapsedMs < 0 {
		t.Errorf("expected non-negative elapsed_ms, got %d", analysis.ElapsedMs)
	}
}

func TestAgentic_CacheHit(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	final := `{"summary":"cached","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"f","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{}
	opts := AgenticOptions{MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second}

	if _, _, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:cached", "sys", "user"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Second call should hit the cache and NOT increment server calls.
	_, a2, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:cached", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Errorf("expected 1 server call (second was cache hit), got %d", got)
	}
	if !a2.CacheHit {
		t.Error("expected cache_hit=true on second (cached) call")
	}
	if a2.ToolCalls != 0 || a2.ModelBytes != 0 || a2.GCSBytes != 0 {
		t.Errorf("expected zero counters on cache hit (no state), got tool_calls=%d model_bytes=%d gcs_bytes=%d",
			a2.ToolCalls, a2.ModelBytes, a2.GCSBytes)
	}
	if a2.Mode != AgenticMode {
		t.Errorf("cache-hit mode = %q, want %q", a2.Mode, AgenticMode)
	}
}

func TestAgentic_ToolsUnsupported_FirstCall(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// 400 with a body that mentions "function calling".
	srv.push(400, `{"error":{"message":"function calling not supported by this model"}}`)

	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second}
	_, _, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, &fakeBrowser{}, opts), "agentic:test:nope", "sys", "user")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrToolsUnsupported) {
		t.Fatalf("expected ErrToolsUnsupported, got: %v", err)
	}
}

func TestAgentic_FinalizeRound_JSONRepair(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Round 1: model returns prose without valid JSON.
	srv.push(200, chatRespFinal("I think it was DNS but I'm not sure."))
	// Finalize round: model returns valid JSON.
	final := `{"summary":"DNS lookup failed","is_transient":false,"root_cause":"resolver","severity":"High","suggested_fix":"fix","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second}
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, &fakeBrowser{}, opts), "agentic:test:repair", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if summary.Summary != "DNS lookup failed" {
		t.Errorf("summary = %q", summary.Summary)
	}
	if analysis.Mode != AgenticMode {
		t.Errorf("mode = %q", analysis.Mode)
	}
}

func TestAgentic_BudgetExhausted_SynthesizesFallback(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Round 1: model returns unparseable prose. Finalize round will also return unparseable prose.
	srv.push(200, chatRespFinal("not json"))
	srv.push(200, chatRespFinal("still not json"))

	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{MaxIters: 1, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second}
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, &fakeBrowser{}, opts), "agentic:test:fallback", "sys", "user")
	if err != nil {
		t.Fatalf("expected fallback synthesis, not error, got: %v", err)
	}
	if summary == nil || analysis == nil {
		t.Fatal("expected synthesized outputs")
	}
	if analysis.Mode != AgenticMode {
		t.Errorf("mode = %q", analysis.Mode)
	}
	// Critically, do NOT cache fallbacks. Re-running should hit the server again.
	srv.push(200, chatRespFinal("still not json"))
	srv.push(200, chatRespFinal("still not json"))
	before := atomic.LoadInt32(&srv.calls)
	if _, _, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, &fakeBrowser{}, opts), "agentic:test:fallback", "sys", "user"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if atomic.LoadInt32(&srv.calls) == before {
		t.Error("fallback should not have been cached (expected server hit on retry)")
	}
}

func TestIsToolsUnsupportedError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain 500", fmt.Errorf("chat returned 500: server error"), false},
		{"400 no tools msg", fmt.Errorf("chat returned 400: bad request"), false},
		{"400 + tools", fmt.Errorf("chat returned 400: tools_choice not supported"), true},
		{"400 + function calling", fmt.Errorf("chat returned 400: function calling not supported"), true},
		{"422 + function_call", fmt.Errorf("chat returned 422: function_call invalid"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isToolsUnsupportedError(tc.err); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTryParseAnalysis(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"empty", "", false},
		{"whitespace", "   \n  ", false},
		{"plain prose", "no json here", false},
		{"valid json", `{"summary":"x","root_cause":"y","severity":"High"}`, true},
		{"json with ```", "```json\n{\"summary\":\"x\",\"root_cause\":\"y\"}\n```", true},
		{"empty fields", `{"summary":"","root_cause":""}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := tryParseAnalysis(tc.in)
			if ok != tc.ok {
				t.Errorf("ok = %v, want %v", ok, tc.ok)
			}
		})
	}
}
