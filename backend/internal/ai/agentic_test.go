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

// ---------- MinToolCalls floor ----------

// TestAgentic_MinToolCalls_NudgeForcesInvestigation verifies the loop refuses
// a tools-free final answer when state.calls < MinToolCalls and instead nudges
// the model. After the nudge, the model issues a tool call and produces a
// final, which is accepted and cached.
func TestAgentic_MinToolCalls_NudgeForcesInvestigation(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Round 1: model tries to finalize immediately with NO tool calls.
	final1 := `{"summary":"made up","is_transient":false,"root_cause":"premature guess","severity":"High","suggested_fix":"x","relevant_files":[]}`
	srv.push(200, chatRespFinal(final1))
	// Round 2 (after nudge): model calls list_artifacts.
	srv.push(200, chatRespToolCall("call_1", "list_artifacts", map[string]interface{}{"path": ""}))
	// Round 3: model finalizes with the post-investigation answer.
	final2 := `{"summary":"real cause","is_transient":false,"root_cause":"found in build-log.txt line 42","severity":"High","suggested_fix":"fix it","relevant_files":["build-log.txt"]}`
	srv.push(200, chatRespFinal(final2))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{
		files: map[string][]byte{"build-log.txt": []byte("the error\n")},
		dirs:  map[string][]string{"": {"artifacts"}},
	}
	opts := AgenticOptions{MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second, MinToolCalls: 1}

	summary, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:nudge1", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Errorf("call count = %d, want 3 (nudged round + tool round + final)", got)
	}
	if summary.Summary != "real cause" {
		t.Errorf("expected post-nudge final, got summary=%q", summary.Summary)
	}
	if analysis.RootCause != "found in build-log.txt line 42" {
		t.Errorf("expected post-nudge root cause, got %q", analysis.RootCause)
	}
	if analysis.ToolCalls != 1 {
		t.Errorf("tool_calls = %d, want 1", analysis.ToolCalls)
	}

	// Floor met (1 >= 1) so the answer should be cached: second call hits
	// the cache and the server is not called again.
	_, _, err = client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:nudge1", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Errorf("call count after cache hit = %d, want 3 (no extra server hit)", got)
	}
}

// TestAgentic_MinToolCalls_StubbornModelAcceptedButNotCached verifies the
// anti-thrash heuristic: if the model returns tools-free twice in a row
// without making any new tool calls between, the loop accepts the answer
// (publication for this run) but does NOT cache it so the next run retries.
func TestAgentic_MinToolCalls_StubbornModelAcceptedButNotCached(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Round 1: tools-free (calls=0 < min=3).
	stubborn := `{"summary":"won't investigate","is_transient":false,"root_cause":"some hypothesis","severity":"Medium","suggested_fix":"y","relevant_files":[]}`
	srv.push(200, chatRespFinal(stubborn))
	// Round 2: tools-free again (still calls=0).
	srv.push(200, chatRespFinal(stubborn))

	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second, MinToolCalls: 3}

	summary, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, &fakeBrowser{}, opts), "agentic:test:stubborn", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Errorf("call count = %d, want 2 (one nudge then accept)", got)
	}
	if summary.Summary != "won't investigate" {
		t.Errorf("expected below-floor final to be published; got %q", summary.Summary)
	}
	if analysis.ToolCalls != 0 {
		t.Errorf("tool_calls = %d, want 0", analysis.ToolCalls)
	}

	// Critically: below-floor answer must NOT be cached. Re-running should
	// hit the server again (not a cache hit).
	srv.push(200, chatRespFinal(stubborn))
	srv.push(200, chatRespFinal(stubborn))
	before := atomic.LoadInt32(&srv.calls)
	_, _, err = client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, &fakeBrowser{}, opts), "agentic:test:stubborn", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if atomic.LoadInt32(&srv.calls) == before {
		t.Error("below-floor final should not have been cached (expected server hit on retry)")
	}
}

// TestAgentic_MinToolCalls_RejectedFinalNotReusedAfterMaxIters is a
// regression for a subtle bug: if the loop rejected a tools-free answer (to
// nudge), the rejected content must not be reused as finalContent after the
// loop exhausts MaxIters. Without the fix, tryParseAnalysis would succeed on
// the stale rejected JSON and the wrong answer would be returned.
func TestAgentic_MinToolCalls_RejectedFinalNotReusedAfterMaxIters(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Round 1: tools-free with valid JSON; we will reject this (calls=0 < min=2).
	rejected := `{"summary":"REJECTED","is_transient":false,"root_cause":"premature","severity":"High","suggested_fix":"x","relevant_files":[]}`
	srv.push(200, chatRespFinal(rejected))
	// Rounds 2+ (after nudge): model only ever calls tools, never finalizes.
	// MaxIters=3 means we get exactly 2 more chat calls. Both are tool calls.
	srv.push(200, chatRespToolCall("call_1", "list_artifacts", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespToolCall("call_2", "list_artifacts", map[string]interface{}{"path": ""}))
	// Loop exits via MaxIters; runFinalizeRound fires. Force a successful
	// finalize so we land in the cache-write path.
	final := `{"summary":"FINAL","is_transient":false,"root_cause":"from finalize round","severity":"High","suggested_fix":"y","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{dirs: map[string][]string{"": {"artifacts"}}}
	opts := AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second, MinToolCalls: 2}

	summary, _, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:notreused", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if summary.Summary == "REJECTED" {
		t.Errorf("rejected pre-nudge JSON leaked into final output: got %q", summary.Summary)
	}
	if summary.Summary != "FINAL" {
		t.Errorf("expected finalize-round output, got %q", summary.Summary)
	}
}

// TestAgentic_MinToolCalls_CacheInvalidatesBelowFloor verifies the cache-read
// path re-validates against the current floor: a cached analysis with
// ToolCalls below the now-configured MinToolCalls is treated as a miss and
// re-analyzed.
func TestAgentic_MinToolCalls_CacheInvalidatesBelowFloor(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// First call with min=0: model finalizes with 0 tool calls. Cached.
	zeroTool := `{"summary":"original","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"s","relevant_files":[]}`
	srv.push(200, chatRespFinal(zeroTool))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{
		files: map[string][]byte{"build-log.txt": []byte("err\n")},
		dirs:  map[string][]string{"": {"artifacts"}},
	}
	noFloor := AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second, MinToolCalls: 0}
	if _, _, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, noFloor), "agentic:test:invalidate", "sys", "user"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Fatalf("expected 1 server call after first analysis, got %d", got)
	}

	// Bump the floor to 2. The cached entry has ToolCalls=0 < 2 → cache miss.
	// Model now does 2 tool calls + a final.
	srv.push(200, chatRespToolCall("c1", "list_artifacts", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespToolCall("c2", "list_artifacts", map[string]interface{}{"path": ""}))
	newFinal := `{"summary":"reanalyzed","is_transient":false,"root_cause":"r2","severity":"High","suggested_fix":"s2","relevant_files":[]}`
	srv.push(200, chatRespFinal(newFinal))

	withFloor := AgenticOptions{MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second, MinToolCalls: 2}
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, withFloor), "agentic:test:invalidate", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if summary.Summary != "reanalyzed" {
		t.Errorf("expected re-analyzed result, got %q (stale cache served)", summary.Summary)
	}
	if analysis.ToolCalls != 2 {
		t.Errorf("tool_calls = %d, want 2", analysis.ToolCalls)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 4 {
		t.Errorf("call count = %d, want 4 (1 first analysis + 3 second analysis)", got)
	}

	// Third call with the same floor=2 should now hit the cache (re-analyzed
	// entry has tool_calls=2 >= floor=2).
	_, hitAnalysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, withFloor), "agentic:test:invalidate", "sys", "user")
	if err != nil {
		t.Fatalf("third call: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 4 {
		t.Errorf("call count after expected cache hit = %d, want 4 (no extra server hit)", got)
	}
	if !hitAnalysis.CacheHit {
		t.Errorf("CacheHit = false, want true")
	}
	// Cache-hit must restore the recorded telemetry from the prior run, not
	// publish zero. Without this stamping, the build-level
	// shouldReanalyze gate sees ToolCalls=0 and re-invalidates forever.
	if hitAnalysis.ToolCalls != 2 {
		t.Errorf("cache-hit ToolCalls = %d, want 2 (cached telemetry must be stamped on hit)", hitAnalysis.ToolCalls)
	}
}

// TestAgentic_MinToolCalls_ZeroFloor_NoBehaviorChange verifies the default
// (MinToolCalls=0) path: a tools-free first response is accepted immediately,
// matching pre-L.3 behavior so existing consumers see no change.
func TestAgentic_MinToolCalls_ZeroFloor_NoBehaviorChange(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	final := `{"summary":"fast","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"s","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second, MinToolCalls: 0}
	if _, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, &fakeBrowser{}, opts), "agentic:test:zerofloor", "sys", "user"); err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	} else if analysis.ToolCalls != 0 {
		t.Errorf("tool_calls = %d, want 0", analysis.ToolCalls)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Errorf("call count = %d, want 1 (no nudge with floor=0)", got)
	}
}

// ---------- MinGCSBytes floor ----------

// bigPayload returns a deterministic byte slice of the requested size, used
// to drive fakeBrowser.Read past the per-test MinGCSBytes floor.
func bigPayload(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte('A' + (i % 26))
	}
	return out
}

// TestAgentic_MinGCSBytes_NudgeForcesMoreReading verifies that a model
// satisfying MinToolCalls but with no GCS bytes read is nudged, and that
// after subsequent reads cross the byte floor the answer is accepted and
// cached.
func TestAgentic_MinGCSBytes_NudgeForcesMoreReading(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Round 1: list_artifacts (BytesFetched=0).
	srv.push(200, chatRespToolCall("c1", "list_artifacts", map[string]interface{}{"path": ""}))
	// Round 2: tools-free finalize with gcsBytes still 0.
	premature := `{"summary":"shallow","is_transient":false,"root_cause":"unknown","severity":"Medium","suggested_fix":"x","relevant_files":[]}`
	srv.push(200, chatRespFinal(premature))
	// Round 3 (after nudge): read_artifact returning 16 KB so gcsBytes
	// crosses the 15 KB floor.
	srv.push(200, chatRespToolCall("c2", "read_artifact", map[string]interface{}{"path": "build-log.txt", "offset": 0, "length": 16384}))
	// Round 4: tools-free with substantive content.
	final := `{"summary":"deep","is_transient":false,"root_cause":"found in build-log.txt:42","severity":"High","suggested_fix":"fix","relevant_files":["build-log.txt"]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{
		files: map[string][]byte{"build-log.txt": bigPayload(30_000)},
		dirs:  map[string][]string{"": {"artifacts"}},
	}
	opts := AgenticOptions{MaxIters: 6, ModelByteBudget: 200_000, GCSByteBudget: 200_000, WallClock: 30 * time.Second, MinGCSBytes: 15_000}

	summary, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:gcsnudge", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if summary.Summary != "deep" {
		t.Errorf("expected post-nudge final, got summary=%q", summary.Summary)
	}
	if analysis.GCSBytes < 15_000 {
		t.Errorf("gcs_bytes = %d, want >= 15000 (floor must have been met before acceptance)", analysis.GCSBytes)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 4 {
		t.Errorf("call count = %d, want 4 (list + premature final + read + final)", got)
	}

	// Floor met → cached. Re-run hits cache.
	_, _, err = client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:gcsnudge", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 4 {
		t.Errorf("call count after cache hit = %d, want 4 (no extra server hit)", got)
	}
}

// TestAgentic_MinGCSBytes_CacheInvalidatesBelowFloor mirrors the
// MinToolCalls cache-invalidation test for the byte floor. A cached
// analysis with gcs_bytes below the now-configured MinGCSBytes is treated
// as a miss and re-analyzed even though tool_calls already met the (zero)
// MinToolCalls floor on the prior run.
func TestAgentic_MinGCSBytes_CacheInvalidatesBelowFloor(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// First call with no floors: one tiny read then finalize. Cached.
	srv.push(200, chatRespToolCall("c1", "list_artifacts", map[string]interface{}{"path": ""}))
	original := `{"summary":"original","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"s","relevant_files":[]}`
	srv.push(200, chatRespFinal(original))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{
		files: map[string][]byte{"build-log.txt": bigPayload(40_000)},
		dirs:  map[string][]string{"": {"artifacts"}},
	}
	noFloor := AgenticOptions{MaxIters: 4, ModelByteBudget: 200_000, GCSByteBudget: 200_000, WallClock: 30 * time.Second}
	if _, _, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, noFloor), "agentic:test:gcsinvalidate", "sys", "user"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Fatalf("expected 2 server calls after first analysis, got %d", got)
	}

	// Bump MinGCSBytes to 20 KB. The cached entry has gcsBytes=0 (list
	// is the only call; BytesFetched=0) → cache miss. Model now reads
	// 16 KB then 16 KB more, crossing the floor.
	srv.push(200, chatRespToolCall("c2", "read_artifact", map[string]interface{}{"path": "build-log.txt", "offset": 0, "length": 16384}))
	srv.push(200, chatRespToolCall("c3", "read_artifact", map[string]interface{}{"path": "build-log.txt", "offset": 16384, "length": 16384}))
	rerun := `{"summary":"reanalyzed","is_transient":false,"root_cause":"r2","severity":"High","suggested_fix":"s2","relevant_files":[]}`
	srv.push(200, chatRespFinal(rerun))

	withFloor := AgenticOptions{MaxIters: 6, ModelByteBudget: 200_000, GCSByteBudget: 200_000, WallClock: 30 * time.Second, MinGCSBytes: 20_000}
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, withFloor), "agentic:test:gcsinvalidate", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if summary.Summary != "reanalyzed" {
		t.Errorf("expected re-analyzed result, got %q (stale cache served)", summary.Summary)
	}
	if analysis.GCSBytes < 20_000 {
		t.Errorf("gcs_bytes = %d, want >= 20000 (floor must have been met)", analysis.GCSBytes)
	}

	// Third call with the same floor should now hit the cache, and the
	// stamped telemetry must include the recorded gcs_bytes so the
	// build-level shouldReanalyze gate doesn't re-invalidate forever.
	_, hitAnalysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, withFloor), "agentic:test:gcsinvalidate", "sys", "user")
	if err != nil {
		t.Fatalf("third call: %v", err)
	}
	if !hitAnalysis.CacheHit {
		t.Errorf("CacheHit = false, want true")
	}
	if hitAnalysis.GCSBytes < 20_000 {
		t.Errorf("cache-hit gcs_bytes = %d, want >= 20000 (cached telemetry must be stamped on hit)", hitAnalysis.GCSBytes)
	}
}

// TestAgentic_MinGCSBytes_StubbornModelAcceptedButNotCached covers the
// pathological case the rubber-duck flagged: a model that keeps returning
// tools-free without making any new tool calls (and therefore without
// growing gcsBytes) must eventually be accepted under the anti-thrash
// rule rather than being nudged until MaxIters. Below-floor finals are
// still published but NOT cached so the next fetcher run gets a fresh
// attempt.
func TestAgentic_MinGCSBytes_StubbornModelAcceptedButNotCached(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	stubborn := `{"summary":"refuses","is_transient":false,"root_cause":"guess","severity":"Medium","suggested_fix":"y","relevant_files":[]}`
	// Round 1: tools-free (calls=0, gcs=0). Nudge.
	srv.push(200, chatRespFinal(stubborn))
	// Round 2: tools-free again (still calls=0, gcs=0). Anti-thrash
	// rule: no progress on the unmet axis → accept.
	srv.push(200, chatRespFinal(stubborn))

	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{MaxIters: 8, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second, MinGCSBytes: 50_000}

	summary, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, &fakeBrowser{}, opts), "agentic:test:gcsstubborn", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Errorf("call count = %d, want 2 (one nudge then accept under anti-thrash)", got)
	}
	if summary.Summary != "refuses" {
		t.Errorf("expected below-floor stubborn final to be published; got %q", summary.Summary)
	}
	if analysis.GCSBytes != 0 {
		t.Errorf("gcs_bytes = %d, want 0", analysis.GCSBytes)
	}

	// Below-floor answer must NOT be cached.
	srv.push(200, chatRespFinal(stubborn))
	srv.push(200, chatRespFinal(stubborn))
	before := atomic.LoadInt32(&srv.calls)
	if _, _, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, &fakeBrowser{}, opts), "agentic:test:gcsstubborn", "sys", "user"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if atomic.LoadInt32(&srv.calls) == before {
		t.Error("below-floor final should not have been cached (expected server hit on retry)")
	}
}

// TestAgentic_MinGCSBytes_ListOnlyLoopNotInfinite is the rubber-duck #7
// regression: a model that keeps calling list_artifacts (incrementing
// calls but returning BytesFetched=0) must NOT trigger an infinite nudge
// loop. Once calls stops progressing the anti-thrash rule kicks in; if
// the model keeps calling tools, the loop exits at MaxIters and runs
// the forced finalize round. Either way, the loop terminates.
func TestAgentic_MinGCSBytes_ListOnlyLoopNotInfinite(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Loop: list (no bytes) → tools-free → list → tools-free → ...
	// With MaxIters=4 and the floor unmet on the bytes axis,
	// we expect: list (iter 1) → tools-free + nudge (iter 2) →
	// list (iter 3) → tools-free + nudge OR accept (iter 4).
	// Because calls progressed each round (the nudge fires every time
	// the model makes a fresh tool call), we should hit MaxIters and
	// then the forced finalize round produces the answer.
	stubborn := `{"summary":"list_only","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"x","relevant_files":[]}`
	srv.push(200, chatRespToolCall("c1", "list_artifacts", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespFinal(stubborn))
	srv.push(200, chatRespToolCall("c2", "list_artifacts", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespFinal(stubborn))
	// Forced finalize round response after MaxIters exhausted.
	finalAfterFinalize := `{"summary":"forced","is_transient":false,"root_cause":"best effort","severity":"Medium","suggested_fix":"x","relevant_files":[]}`
	srv.push(200, chatRespFinal(finalAfterFinalize))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{dirs: map[string][]string{"": {"artifacts"}}}
	opts := AgenticOptions{MaxIters: 4, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second, MinGCSBytes: 50_000}

	summary, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:listloop", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	// Either acceptance path is fine for the regression: the test
	// passes as long as the loop terminates without panicking and
	// without making more than MaxIters+1 server calls (the +1 is
	// the optional finalize round).
	if got := atomic.LoadInt32(&srv.calls); got > int32(opts.MaxIters+1) {
		t.Errorf("call count = %d, want <= %d (loop must terminate)", got, opts.MaxIters+1)
	}
	if summary == nil || analysis == nil {
		t.Fatalf("expected non-nil outputs even from list-only loop")
	}
	if analysis.GCSBytes != 0 {
		t.Errorf("gcs_bytes = %d, want 0 (list_artifacts returns no bytes)", analysis.GCSBytes)
	}
}

// TestAgToolDocs_L4Step1Anchors pins the L.4 Step 1 anti-punt, drill-down,
// and self-check language in agToolDocs. These bullets exist specifically
// to counter weaker models' tendency to write investigation TODOs into
// suggested_fix instead of running the investigations themselves with the
// tools, so silent deletion would regress the qualitative gap closed by
// L.4. Bump deliberately if rewording.
func TestAgToolDocs_L4Step1Anchors(t *testing.T) {
	required := []string{
		"Drill into the most relevant named resources",
		"pick the 1-3 most directly tied to the failure",
		"Investigation is YOUR job",
		"diagnostic or information-gathering task",
		"still cannot identify a concrete remediation",
		"Do not invoke this escape hatch",
		"Before finalizing, self-check",
	}
	for _, s := range required {
		if !strings.Contains(agToolDocs, s) {
			t.Errorf("agToolDocs missing required L.4 Step 1 anchor %q\nfull text:\n%s", s, agToolDocs)
		}
	}

	// Forbidden: the old "Prefer 3-5 focused calls" line directly
	// contradicted the L.3 min_tool_calls floor by telling the model to
	// stop early. It must stay gone.
	forbidden := []string{
		"Prefer 3-5 focused calls",
		"3-5 focused calls",
	}
	for _, s := range forbidden {
		if strings.Contains(agToolDocs, s) {
			t.Errorf("agToolDocs still contains forbidden pre-L.4 phrase %q\nfull text:\n%s", s, agToolDocs)
		}
	}
}

// TestResponseFormatFooter_L4Step1Anchors pins the L.4 Step 1 tightening of
// the suggested_fix and root_cause schema descriptions. The footer is
// shared by agentic and non-agentic consumers, so the wording must stay
// tool-neutral: it forbids diagnostic tasks in suggested_fix and pushes
// for the underlying cause in root_cause without assuming tools are
// available. Tool-specific enforcement lives in agToolDocs.
func TestResponseFormatFooter_L4Step1Anchors(t *testing.T) {
	required := []string{
		"concrete remediation",
		"Do not list diagnostic or information-gathering tasks",
		"trace the chain back to the underlying cause",
		"No remediation possible from available evidence",
		"available evidence allows",
	}
	for _, s := range required {
		if !strings.Contains(ResponseFormatFooter, s) {
			t.Errorf("ResponseFormatFooter missing required L.4 Step 1 anchor %q\nfull text:\n%s", s, ResponseFormatFooter)
		}
	}

	// Forbidden: the old terse "exact fix with file paths and changes
	// needed" description gave models no signal that investigation
	// verbs were off-limits. Must stay rewritten.
	forbidden := []string{
		"exact fix with file paths and changes needed",
	}
	for _, s := range forbidden {
		if strings.Contains(ResponseFormatFooter, s) {
			t.Errorf("ResponseFormatFooter still contains pre-L.4 phrase %q\nfull text:\n%s", s, ResponseFormatFooter)
		}
	}

	// The footer is shared with non-agentic consumers (e.g. the generic
	// module that ships prebuilt evidence with no tools wired), so any
	// language asserting tools must be used would be literally false in
	// that mode. Keep tool-specific enforcement in agToolDocs only.
	toolSpecificForbidden := []string{
		"using your tools",
		"with the tools",
		"Investigation is the agent's job",
	}
	for _, s := range toolSpecificForbidden {
		if strings.Contains(ResponseFormatFooter, s) {
			t.Errorf("ResponseFormatFooter leaked tool-specific phrase %q (footer is shared with non-agentic consumers; keep tool wording in agToolDocs)\nfull text:\n%s", s, ResponseFormatFooter)
		}
	}
}

// ---------- L.4 Step 2 critique gate ----------
//
// A "punt-shaped" suggested_fix is a diagnostic / information-gathering
// TODO list ("Check X. Verify Y. Investigate Z.") instead of a concrete
// remediation. Critique catches this pattern and re-prompts the model.
// See critique.go for the regex; these tests exercise the loop integration.

const puntyFinalJSON = `{"summary":"shallow","is_transient":false,"root_cause":"third CP machine cloud-init empty","severity":"High","suggested_fix":"Check the AzureMachine status conditions. Verify cloud-init script execution. Investigate Azure activity logs.","relevant_files":[]}`

const cleanFinalJSON = `{"summary":"deep","is_transient":false,"root_cause":"third CP machine cloud-init empty due to vnet peering mismatch","severity":"High","suggested_fix":"Update kustomize/cluster-template.yaml line 142 to match the staging vnet peering name; reapply and retry.","relevant_files":["kustomize/cluster-template.yaml"]}`

// TestAgentic_Critique_FailRetryPass exercises the happy retry path: the
// model returns a punt-shaped final, critique fails, the loop appends
// feedback and re-prompts, the model returns a clean fix, critique passes,
// and the answer is cached.
func TestAgentic_Critique_FailRetryPass(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Round 1: punt-shaped final → critique fails → re-prompt.
	srv.push(200, chatRespFinal(puntyFinalJSON))
	// Round 2 (after critique feedback): clean final → critique passes.
	srv.push(200, chatRespFinal(cleanFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{
		MaxIters:           5,
		ModelByteBudget:    100_000,
		GCSByteBudget:      100_000,
		WallClock:          30 * time.Second,
		CritiqueEnabled:    true,
		CritiqueMaxRetries: 2,
	}
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, opts),
		"agentic:test:critique-pass", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Errorf("call count = %d, want 2 (punt + retry)", got)
	}
	if summary.Summary != "deep" {
		t.Errorf("expected clean final, got summary=%q", summary.Summary)
	}
	if !strings.Contains(analysis.SuggestedFix, "kustomize/cluster-template.yaml") {
		t.Errorf("expected concrete fix, got %q", analysis.SuggestedFix)
	}

	// Critique-passing answer must be cached: second call hits cache, no
	// extra server hit.
	_, hit, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, opts),
		"agentic:test:critique-pass", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Errorf("call count after cache hit = %d, want 2 (no extra server hit)", got)
	}
	if !hit.CacheHit {
		t.Errorf("CacheHit = false, want true")
	}
}

// TestAgentic_Critique_ExhaustedAcceptedNotCached verifies the anti-thrash
// behavior: a model that keeps emitting punts exhausts its retries, the
// last draft is published, but it is NOT cached so the next run gets a
// fresh attempt (same pattern as MinToolCalls stubborn-model).
func TestAgentic_Critique_ExhaustedAcceptedNotCached(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Three punts: original + 2 retries (CritiqueMaxRetries=2). All fail
	// critique. Loop accepts the last one and skips the cache write.
	srv.push(200, chatRespFinal(puntyFinalJSON))
	srv.push(200, chatRespFinal(puntyFinalJSON))
	srv.push(200, chatRespFinal(puntyFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{
		MaxIters:           5,
		ModelByteBudget:    100_000,
		GCSByteBudget:      100_000,
		WallClock:          30 * time.Second,
		CritiqueEnabled:    true,
		CritiqueMaxRetries: 2,
	}
	summary, _, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, opts),
		"agentic:test:critique-exhausted", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Errorf("call count = %d, want 3 (original + 2 retries)", got)
	}
	if summary.Summary != "shallow" {
		t.Errorf("expected punt-shaped final to be published, got %q", summary.Summary)
	}

	// Critique-failing answer must NOT be cached. Push three more punts
	// and re-run; we should see all three consumed (cache miss → server
	// hit on second analysis too).
	srv.push(200, chatRespFinal(puntyFinalJSON))
	srv.push(200, chatRespFinal(puntyFinalJSON))
	srv.push(200, chatRespFinal(puntyFinalJSON))
	before := atomic.LoadInt32(&srv.calls)
	_, _, err = client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, opts),
		"agentic:test:critique-exhausted", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if atomic.LoadInt32(&srv.calls) == before {
		t.Error("punt-shaped final should not have been cached (expected server hits on retry)")
	}
}

// TestAgentic_Critique_Disabled_NoBehaviorChange verifies the default
// (CritiqueEnabled=false) path: a punt-shaped final is accepted as-is
// and cached, matching pre-L.4-Step-2 behavior so consumers that don't
// opt in see no change.
func TestAgentic_Critique_Disabled_NoBehaviorChange(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(puntyFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{
		MaxIters:        5,
		ModelByteBudget: 100_000,
		GCSByteBudget:   100_000,
		WallClock:       30 * time.Second,
		// CritiqueEnabled defaults to false.
	}
	summary, _, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, opts),
		"agentic:test:critique-disabled", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Errorf("call count = %d, want 1 (no retry with critique off)", got)
	}
	if summary.Summary != "shallow" {
		t.Errorf("expected unmodified punt-shaped final, got %q", summary.Summary)
	}

	// Cached: second call must hit cache (no critique gate when disabled).
	_, hit, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, opts),
		"agentic:test:critique-disabled", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Errorf("call count after cache hit = %d, want 1", got)
	}
	if !hit.CacheHit {
		t.Errorf("CacheHit = false, want true")
	}
}

// TestAgentic_Critique_CacheInvalidatesUncritiqued verifies the cache
// invalidation path: an entry cached while critique was disabled (or
// pre-Step-2) has CritiquePassed=false. When a consumer later enables
// critique, that entry must be treated as a cache miss and re-analyzed.
// Mirrors TestAgentic_MinToolCalls_CacheInvalidatesBelowFloor for floors.
func TestAgentic_Critique_CacheInvalidatesUncritiqued(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// First call: critique disabled, model emits a punt-shaped final, cached.
	srv.push(200, chatRespFinal(puntyFinalJSON))
	client := newAgenticTestClient(t, srv.URL)
	off := AgenticOptions{MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, WallClock: 30 * time.Second}
	if _, _, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, off),
		"agentic:test:critique-invalidate", "sys", "user"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Fatalf("first call should hit server once, got %d", got)
	}

	// Enable critique. Cached CritiquePassed=false → invalidate → re-analyze.
	srv.push(200, chatRespFinal(puntyFinalJSON))
	srv.push(200, chatRespFinal(cleanFinalJSON))
	on := AgenticOptions{
		MaxIters:           5,
		ModelByteBudget:    100_000,
		GCSByteBudget:      100_000,
		WallClock:          30 * time.Second,
		CritiqueEnabled:    true,
		CritiqueMaxRetries: 2,
	}
	summary, _, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, on),
		"agentic:test:critique-invalidate", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if summary.Summary != "deep" {
		t.Errorf("expected re-analyzed clean final, got %q (stale cache served)", summary.Summary)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Errorf("call count = %d, want 3 (1 first + punt-retry-clean for second)", got)
	}
}

// TestAgentic_Critique_FinalizeRoundOutputCritiqued verifies the
// rubber-duck-#1 fix: when the loop maxes out without ever returning a
// tools-free message, runFinalizeRound produces the final answer. That
// output must be critique-checked too, otherwise a punt-shaped
// finalize-round result publishes-but-never-caches and re-analyzes on
// every fetcher run (cost blow-up).
func TestAgentic_Critique_FinalizeRoundOutputCritiqued(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// MaxIters=2: model only ever calls tools, never finalizes inside
	// the loop. Loop exits via MaxIters → runFinalizeRound fires.
	srv.push(200, chatRespToolCall("c1", "list_artifacts", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespToolCall("c2", "list_artifacts", map[string]interface{}{"path": ""}))
	// runFinalizeRound: model emits a clean (non-punt) final.
	srv.push(200, chatRespFinal(cleanFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{dirs: map[string][]string{"": {"artifacts"}}}
	opts := AgenticOptions{
		MaxIters:           2,
		ModelByteBudget:    100_000,
		GCSByteBudget:      100_000,
		WallClock:          30 * time.Second,
		CritiqueEnabled:    true,
		CritiqueMaxRetries: 2,
	}
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, browser, opts),
		"agentic:test:finalize-clean", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if summary.Summary != "deep" {
		t.Errorf("expected finalize-round clean answer, got %q", summary.Summary)
	}
	// The clean answer must have passed the post-loop critique check
	// AND been stamped onto AIAnalysis so the build-cache layer can
	// serve it without re-invalidating next run.
	if !analysis.CritiquePassed {
		t.Errorf("CritiquePassed = false, want true (finalize-round clean answer must be critique-checked)")
	}

	// Cache: a critique-passing finalize-round answer must cache
	// normally. Without the post-loop critique check, state.critiquePassed
	// would stay false → cache write blocked → next run re-analyzes.
	// With the fix, cache hits and the server is not called again.
	_, hit, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, browser, opts),
		"agentic:test:finalize-clean", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Errorf("call count after cache hit = %d, want 3 (no extra server hit)", got)
	}
	if !hit.CacheHit {
		t.Errorf("CacheHit = false, want true")
	}
	if !hit.CritiquePassed {
		t.Errorf("cache-hit CritiquePassed = false, want true (cached telemetry must round-trip)")
	}
}

// TestAgentic_Critique_FinalizeRoundPuntNotCached is the negative
// counterpart: a finalize-round result that punts must NOT cache, but
// must publish so triage still has something. Same cost-leak guard as
// the in-loop exhausted-retry path.
func TestAgentic_Critique_FinalizeRoundPuntNotCached(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	srv.push(200, chatRespToolCall("c1", "list_artifacts", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespToolCall("c2", "list_artifacts", map[string]interface{}{"path": ""}))
	// runFinalizeRound: model emits a punt-shaped final → critique fails.
	srv.push(200, chatRespFinal(puntyFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{dirs: map[string][]string{"": {"artifacts"}}}
	opts := AgenticOptions{
		MaxIters:           2,
		ModelByteBudget:    100_000,
		GCSByteBudget:      100_000,
		WallClock:          30 * time.Second,
		CritiqueEnabled:    true,
		CritiqueMaxRetries: 2,
	}
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, browser, opts),
		"agentic:test:finalize-punt", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if summary.Summary != "shallow" {
		t.Errorf("expected punt-shaped finalize-round answer to be published, got %q", summary.Summary)
	}
	if analysis.CritiquePassed {
		t.Errorf("CritiquePassed = true, want false (punt-shaped finalize-round output)")
	}

	// Must not cache: re-running consumes server again.
	srv.push(200, chatRespToolCall("c3", "list_artifacts", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespToolCall("c4", "list_artifacts", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespFinal(puntyFinalJSON))
	before := atomic.LoadInt32(&srv.calls)
	_, _, err = client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, browser, opts),
		"agentic:test:finalize-punt", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if atomic.LoadInt32(&srv.calls) == before {
		t.Error("punt-shaped finalize-round answer should not have been cached (expected server hits on retry)")
	}
}

// TestAgentic_Critique_RetryAllowsToolCallThenFinal verifies the
// rubber-duck-#2 fix: when the model responds to critique feedback with
// a tool call before re-emitting, the bumped maxIters must have room
// for the tool round AND the new final. With the old maxIters++ this
// test would fail because the tool call consumed the only extra
// iteration and the re-final would fall into runFinalizeRound.
func TestAgentic_Critique_RetryAllowsToolCallThenFinal(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Round 1: model emits a punt-shaped final → critique fails →
	// re-prompt. With critiqueRetryIters=3 the loop now has room for
	// the tool call + clean final, without resorting to runFinalizeRound.
	srv.push(200, chatRespFinal(puntyFinalJSON))
	// Round 2: model reads an artifact in response to critique feedback.
	srv.push(200, chatRespToolCall("c1", "read_artifact", map[string]interface{}{
		"path": "build-log.txt", "offset": 0, "length": 256,
	}))
	// Round 3: model re-emits with the clean final → critique passes.
	srv.push(200, chatRespFinal(cleanFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{
		files: map[string][]byte{"build-log.txt": []byte("the error context\n")},
		dirs:  map[string][]string{"": {"artifacts"}},
	}
	// MaxIters=1 so we ONLY have room for one initial iteration; the
	// critique retry budget must do the rest. Without the +3 retry
	// bump, the tool call (round 2) would max us out and force a
	// finalize-round which would bypass the in-loop critique state.
	opts := AgenticOptions{
		MaxIters:           1,
		ModelByteBudget:    100_000,
		GCSByteBudget:      100_000,
		WallClock:          30 * time.Second,
		CritiqueEnabled:    true,
		CritiqueMaxRetries: 1,
	}
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, browser, opts),
		"agentic:test:critique-toolcall", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if summary.Summary != "deep" {
		t.Errorf("expected clean re-emit after tool call, got %q", summary.Summary)
	}
	if !analysis.CritiquePassed {
		t.Errorf("CritiquePassed = false, want true (critique should have passed after tool-call retry)")
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Errorf("call count = %d, want 3 (punt + tool + clean)", got)
	}
}

// TestCritiqueDraft_FeedbackTruncatesLongFix verifies rubber-duck-#5:
// a pathologically long suggested_fix must be truncated in the quoted
// portion of the feedback message so retries don't balloon the
// conversation history. Matched phrases are still listed separately
// and not truncated.
func TestCritiqueDraft_FeedbackTruncatesLongFix(t *testing.T) {
	// Build a long fix that triggers the punt regex.
	prefix := "Check the AzureMachine status. "
	long := prefix + strings.Repeat("Additional details and context. ", 200)
	if len(long) <= feedbackQuoteLimit {
		t.Fatalf("test setup: long fix is too short (%d <= %d)", len(long), feedbackQuoteLimit)
	}
	out := critiqueDraft(analysisResponse{SuggestedFix: long})
	if out.Passed {
		t.Fatalf("expected punt")
	}
	if !strings.Contains(out.Feedback, "… [truncated]") {
		t.Errorf("Feedback missing truncation marker for long fix\nlen(feedback)=%d", len(out.Feedback))
	}
	// Truncated quote is bounded; the rest of the feedback template is
	// fixed-size, so the total length should not grow linearly with the
	// input. Empirical bound: template ~1500 chars + quote limit + match
	// list. Leave generous slack.
	if got := len(out.Feedback); got > feedbackQuoteLimit+3000 {
		t.Errorf("Feedback unexpectedly long: %d chars (limit ~%d)", got, feedbackQuoteLimit+3000)
	}
}
