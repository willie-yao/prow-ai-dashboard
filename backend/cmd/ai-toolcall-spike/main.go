// Spike: validate whether OpenAI-style function calling on the Copilot endpoint
// can produce equal-or-better debugging analyses than today's curator-driven
// evidence pipeline.
//
// THROWAWAY. Do not import from this package. If the spike succeeds the loop +
// tool definitions get rewritten as proper ai.AgenticClient and ai.Tools, with
// validated path handling, real caching, and integration with project config.
//
// Run with:
//
//	AI_TOKEN=$GITHUB_TOKEN \
//	go run ./backend/cmd/ai-toolcall-spike \
//	  -job periodic-cluster-api-e2e-main \
//	  -build 2060033273945395200 \
//	  -test '[It] When testing Cluster API ... [ClusterClass]' \
//	  -failure 'Timed out after 300.001s. Failed to verify Machines Ready ...'
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

// ---------- Config ----------

type config struct {
	Bucket          string
	Job             string
	Build           string
	TestName        string
	FailureMessage  string
	Endpoint        string
	Model           string
	MaxIters        int
	ModelByteBudget int
	GCSByteBudget   int
	WallClock       time.Duration
	Verbose         bool
}

func parseFlags() *config {
	c := &config{}
	flag.StringVar(&c.Bucket, "bucket", "kubernetes-ci-logs", "GCS bucket holding prow artifacts")
	flag.StringVar(&c.Job, "job", "", "prow job name (required)")
	flag.StringVar(&c.Build, "build", "", "prow build ID (required)")
	flag.StringVar(&c.TestName, "test", "", "failing test name to investigate")
	flag.StringVar(&c.FailureMessage, "failure", "", "failure message / stack trace excerpt")
	flag.StringVar(&c.Endpoint, "endpoint", "https://api.githubcopilot.com/chat/completions", "chat completions endpoint")
	flag.StringVar(&c.Model, "model", "claude-opus-4.7-xhigh", "model identifier")
	flag.IntVar(&c.MaxIters, "max-iters", 10, "max agent loop iterations")
	flag.IntVar(&c.ModelByteBudget, "model-bytes", 300_000, "max bytes returned to model across tool calls")
	flag.IntVar(&c.GCSByteBudget, "gcs-bytes", 1_000_000_000, "max bytes fetched from GCS (1GB)")
	flag.DurationVar(&c.WallClock, "wall-clock", 10*time.Minute, "max total wall-clock time")
	flag.BoolVar(&c.Verbose, "v", false, "log every tool call + arguments")
	flag.Parse()
	if c.Job == "" || c.Build == "" {
		fmt.Fprintln(os.Stderr, "usage: -job and -build are required")
		flag.Usage()
		os.Exit(2)
	}
	return c
}

// ---------- Chat types ----------

type chatMessage struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function function `json:"function"`
}

type function struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolDef struct {
	Type     string   `json:"type"`
	Function toolFunc `json:"function"`
}

type toolFunc struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Tools    []toolDef     `json:"tools,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		FinishReason string      `json:"finish_reason"`
		Message      chatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func strPtr(s string) *string { return &s }

// ---------- Agent state ----------

// agentState carries the per-run budgets, file cache, and counters. Passed by
// pointer into every tool so they can deduct from budgets and cache fetches.
type agentState struct {
	cfg          *config
	httpClient   *http.Client
	startTime    time.Time
	cache        map[string][]byte // path -> full file bytes (capped per file)
	modelBytes   int               // bytes returned to model so far
	gcsBytes     int               // bytes fetched from GCS so far
	toolCallsBy  map[string]int    // per-tool counters
	promptTokens int
	complTokens  int
	roundTrips   int
}

func newState(cfg *config) *agentState {
	return &agentState{
		cfg:         cfg,
		httpClient:  &http.Client{Timeout: 90 * time.Second},
		startTime:   time.Now(),
		cache:       map[string][]byte{},
		toolCallsBy: map[string]int{},
	}
}

func (s *agentState) budgetRemainingModel() int { return s.cfg.ModelByteBudget - s.modelBytes }
func (s *agentState) budgetRemainingGCS() int   { return s.cfg.GCSByteBudget - s.gcsBytes }
func (s *agentState) wallClockRemaining() time.Duration {
	return s.cfg.WallClock - time.Since(s.startTime)
}

// ---------- Path safety ----------

var dangerousRe = regexp.MustCompile(`[\x00-\x1f\\]`)

func safePath(userPath string) (string, error) {
	if strings.ContainsAny(userPath, "\x00") {
		return "", fmt.Errorf("path contains NUL byte")
	}
	if dangerousRe.MatchString(userPath) {
		return "", fmt.Errorf("path contains backslash or control character")
	}
	if strings.HasPrefix(userPath, "/") {
		return "", fmt.Errorf("path must be relative to build root (no leading /)")
	}
	if strings.Contains(userPath, "://") {
		return "", fmt.Errorf("path looks like a URL")
	}
	cleaned := path.Clean("/" + userPath)
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "." {
		cleaned = ""
	}
	if strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("path escapes build root")
	}
	return cleaned, nil
}

func (s *agentState) buildBase() string {
	return fmt.Sprintf("https://storage.googleapis.com/%s/logs/%s/%s/", s.cfg.Bucket, s.cfg.Job, s.cfg.Build)
}

func (s *agentState) buildPrefix() string {
	return fmt.Sprintf("logs/%s/%s/", s.cfg.Job, s.cfg.Build)
}

// ---------- Tools ----------

func toolDefs() []toolDef {
	return []toolDef{
		{
			Type: "function",
			Function: toolFunc{
				Name:        "list_artifacts",
				Description: "List the immediate children of a directory in the build's GCS artifact tree. Pass an empty string for the build root. Returns dirs and files (with sizes).",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Directory path relative to the build root, e.g. \"\" for root, \"artifacts/\", \"artifacts/clusters/foo/machines/bar/\". Always end directory paths with /.",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: toolFunc{
				Name:        "read_artifact",
				Description: "Read a byte range of a file. Use for small/known files. For large logs prefer tail_artifact or grep_artifact. Returns up to 16384 bytes per call.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path":   map[string]interface{}{"type": "string", "description": "File path relative to build root."},
						"offset": map[string]interface{}{"type": "integer", "description": "Byte offset to start reading from (default 0).", "default": 0},
						"length": map[string]interface{}{"type": "integer", "description": "Number of bytes to read (default 8192, max 16384).", "default": 8192},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: toolFunc{
				Name:        "tail_artifact",
				Description: "Return the last N lines of a file. Most efficient way to inspect the end of a build log or controller log. Default 500 lines, max 2000.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path":  map[string]interface{}{"type": "string", "description": "File path relative to build root."},
						"lines": map[string]interface{}{"type": "integer", "description": "Number of trailing lines (default 500, max 2000).", "default": 500},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: toolFunc{
				Name:        "grep_artifact",
				Description: "Regex-search a file for matching lines. Returns matches with surrounding context lines and line numbers. Use this for huge build-logs where you want to find specific errors.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path":          map[string]interface{}{"type": "string", "description": "File path relative to build root."},
						"pattern":       map[string]interface{}{"type": "string", "description": "RE2 regex (Go syntax). Use (?i) prefix for case-insensitive."},
						"context_lines": map[string]interface{}{"type": "integer", "description": "Lines of context before/after each match (default 2, max 5).", "default": 2},
						"max_matches":   map[string]interface{}{"type": "integer", "description": "Max matches to return (default 30, max 100).", "default": 30},
					},
					"required": []string{"path", "pattern"},
				},
			},
		},
	}
}

// dispatchTool executes the named tool and returns the JSON-encoded result that
// will be sent back to the model. All errors become JSON `{"error": "..."}` so
// the model can recover. Hard runtime errors (network, panic) propagate.
func dispatchTool(ctx context.Context, s *agentState, call toolCall) string {
	s.toolCallsBy[call.Function.Name]++

	switch call.Function.Name {
	case "list_artifacts":
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return toolErr(s, "invalid arguments: "+err.Error())
		}
		return toolListArtifacts(ctx, s, args.Path)
	case "read_artifact":
		var args struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Length int    `json:"length"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return toolErr(s, "invalid arguments: "+err.Error())
		}
		if args.Length == 0 {
			args.Length = 8192
		}
		if args.Length > 16384 {
			args.Length = 16384
		}
		return toolReadArtifact(ctx, s, args.Path, args.Offset, args.Length)
	case "tail_artifact":
		var args struct {
			Path  string `json:"path"`
			Lines int    `json:"lines"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return toolErr(s, "invalid arguments: "+err.Error())
		}
		if args.Lines == 0 {
			args.Lines = 500
		}
		if args.Lines > 2000 {
			args.Lines = 2000
		}
		return toolTailArtifact(ctx, s, args.Path, args.Lines)
	case "grep_artifact":
		var args struct {
			Path         string `json:"path"`
			Pattern      string `json:"pattern"`
			ContextLines int    `json:"context_lines"`
			MaxMatches   int    `json:"max_matches"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return toolErr(s, "invalid arguments: "+err.Error())
		}
		if args.ContextLines == 0 {
			args.ContextLines = 2
		}
		if args.ContextLines > 5 {
			args.ContextLines = 5
		}
		if args.MaxMatches == 0 {
			args.MaxMatches = 30
		}
		if args.MaxMatches > 100 {
			args.MaxMatches = 100
		}
		return toolGrepArtifact(ctx, s, args.Path, args.Pattern, args.ContextLines, args.MaxMatches)
	default:
		return toolErr(s, "unknown tool: "+call.Function.Name)
	}
}

func toolEnvelope(s *agentState, payload map[string]interface{}) string {
	payload["remaining_model_bytes"] = s.budgetRemainingModel()
	payload["remaining_gcs_bytes"] = s.budgetRemainingGCS()
	b, _ := json.Marshal(payload)
	s.modelBytes += len(b)
	return string(b)
}

func toolErr(s *agentState, msg string) string {
	return toolEnvelope(s, map[string]interface{}{"error": msg})
}

func toolListArtifacts(ctx context.Context, s *agentState, userPath string) string {
	clean, err := safePath(userPath)
	if err != nil {
		return toolErr(s, err.Error())
	}
	if clean != "" && !strings.HasSuffix(clean, "/") {
		clean += "/"
	}
	prefix := s.buildPrefix() + clean
	apiURL := fmt.Sprintf("https://storage.googleapis.com/storage/v1/b/%s/o?prefix=%s&delimiter=/&maxResults=1000",
		s.cfg.Bucket, urlEscape(prefix))

	if s.budgetRemainingGCS() < 1024 {
		return toolErr(s, "gcs byte budget exhausted")
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return toolErr(s, "fetch listing: "+err.Error())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s.gcsBytes += len(body)
	if resp.StatusCode != http.StatusOK {
		return toolErr(s, fmt.Sprintf("listing returned %d", resp.StatusCode))
	}

	var raw struct {
		Items []struct {
			Name string `json:"name"`
			Size string `json:"size"`
		} `json:"items"`
		Prefixes      []string `json:"prefixes"`
		NextPageToken string   `json:"nextPageToken"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return toolErr(s, "parse listing: "+err.Error())
	}

	var dirs []string
	for _, p := range raw.Prefixes {
		dirs = append(dirs, strings.TrimPrefix(p, prefix))
	}
	type fileRow struct {
		Name string `json:"name"`
		Size string `json:"size"`
	}
	var files []fileRow
	for _, it := range raw.Items {
		files = append(files, fileRow{
			Name: strings.TrimPrefix(it.Name, prefix),
			Size: it.Size,
		})
	}

	payload := map[string]interface{}{
		"path":  clean,
		"dirs":  dirs,
		"files": files,
	}
	if raw.NextPageToken != "" {
		payload["truncated"] = true
		payload["truncation_note"] = "more than 1000 entries; only first page returned"
	}
	return toolEnvelope(s, payload)
}

// fetchFullFile returns the cached bytes for a file (fetching once on miss).
// Caps a single file at 250MB; larger files fail the tool call gracefully.
const maxCacheableFileBytes = 250 * 1024 * 1024

func (s *agentState) fetchFullFile(ctx context.Context, relPath string) ([]byte, error) {
	if data, ok := s.cache[relPath]; ok {
		return data, nil
	}
	if s.budgetRemainingGCS() < 1024 {
		return nil, fmt.Errorf("gcs byte budget exhausted")
	}
	rawURL := s.buildBase() + urlEscapePath(relPath)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, relPath)
	}
	lr := io.LimitReader(resp.Body, maxCacheableFileBytes+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if len(data) > maxCacheableFileBytes {
		return nil, fmt.Errorf("file exceeds %dMB cache cap; use read_artifact with byte ranges instead", maxCacheableFileBytes/1024/1024)
	}
	s.gcsBytes += len(data)
	s.cache[relPath] = data
	return data, nil
}

func toolReadArtifact(ctx context.Context, s *agentState, userPath string, offset, length int) string {
	clean, err := safePath(userPath)
	if err != nil {
		return toolErr(s, err.Error())
	}
	if clean == "" {
		return toolErr(s, "path is required")
	}
	if s.budgetRemainingModel() < 256 {
		return toolErr(s, "model byte budget exhausted; produce final answer")
	}

	// If file is already cached, slice from cache.
	if data, ok := s.cache[clean]; ok {
		return readSlice(s, clean, data, offset, length, len(data))
	}

	// For small/unknown files, use HTTP Range to avoid pulling the whole thing.
	if s.budgetRemainingGCS() < length {
		return toolErr(s, "gcs byte budget exhausted")
	}
	rawURL := s.buildBase() + urlEscapePath(clean)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return toolErr(s, "fetch: "+err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return toolErr(s, fmt.Sprintf("HTTP %d for %s", resp.StatusCode, clean))
	}
	body, _ := io.ReadAll(resp.Body)
	s.gcsBytes += len(body)

	totalSize := -1
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		// bytes 0-8191/156504940
		if i := strings.LastIndex(cr, "/"); i >= 0 {
			fmt.Sscanf(cr[i+1:], "%d", &totalSize)
		}
	}
	return readSlice(s, clean, body, 0, length, totalSize)
}

func readSlice(s *agentState, p string, data []byte, offset, length, totalSize int) string {
	if offset < 0 {
		offset = 0
	}
	if offset > len(data) {
		return toolErr(s, fmt.Sprintf("offset %d beyond data length %d", offset, len(data)))
	}
	end := offset + length
	if end > len(data) {
		end = len(data)
	}
	chunk := data[offset:end]
	payload := map[string]interface{}{
		"path":         p,
		"offset":       offset,
		"bytes_read":   len(chunk),
		"file_size":    totalSize,
		"content":      string(chunk),
		"more_after":   totalSize > end,
	}
	return toolEnvelope(s, payload)
}

func toolTailArtifact(ctx context.Context, s *agentState, userPath string, lines int) string {
	clean, err := safePath(userPath)
	if err != nil {
		return toolErr(s, err.Error())
	}
	if clean == "" {
		return toolErr(s, "path is required")
	}
	if s.budgetRemainingModel() < 256 {
		return toolErr(s, "model byte budget exhausted; produce final answer")
	}
	data, err := s.fetchFullFile(ctx, clean)
	if err != nil {
		return toolErr(s, err.Error())
	}
	all := bytes.Split(data, []byte("\n"))
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	out := bytes.Join(all, []byte("\n"))
	// Hard cap at 32KB regardless of line count to bound model bytes.
	const cap = 32 * 1024
	if len(out) > cap {
		out = out[len(out)-cap:]
	}
	payload := map[string]interface{}{
		"path":         clean,
		"file_size":    len(data),
		"lines_returned": min(lines, len(all)),
		"content":      string(out),
	}
	return toolEnvelope(s, payload)
}

func toolGrepArtifact(ctx context.Context, s *agentState, userPath, pattern string, contextLines, maxMatches int) string {
	clean, err := safePath(userPath)
	if err != nil {
		return toolErr(s, err.Error())
	}
	if clean == "" {
		return toolErr(s, "path is required")
	}
	if s.budgetRemainingModel() < 256 {
		return toolErr(s, "model byte budget exhausted; produce final answer")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return toolErr(s, "invalid regex: "+err.Error())
	}
	data, err := s.fetchFullFile(ctx, clean)
	if err != nil {
		return toolErr(s, err.Error())
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	var allLines []string
	for scanner.Scan() {
		// Cap line length to avoid pathological lines blowing out budget.
		l := scanner.Text()
		if len(l) > 1000 {
			l = l[:1000] + "...<truncated>"
		}
		allLines = append(allLines, l)
	}

	type match struct {
		LineNo  int      `json:"line_no"`
		Context []string `json:"context"`
	}
	var matches []match
	totalMatches := 0
	for i, line := range allLines {
		if !re.MatchString(line) {
			continue
		}
		totalMatches++
		if len(matches) >= maxMatches {
			continue
		}
		start := i - contextLines
		if start < 0 {
			start = 0
		}
		end := i + contextLines + 1
		if end > len(allLines) {
			end = len(allLines)
		}
		ctxLines := make([]string, 0, end-start)
		for j := start; j < end; j++ {
			marker := "  "
			if j == i {
				marker = "> "
			}
			ctxLines = append(ctxLines, fmt.Sprintf("%s%d: %s", marker, j+1, allLines[j]))
		}
		matches = append(matches, match{LineNo: i + 1, Context: ctxLines})
	}
	payload := map[string]interface{}{
		"path":           clean,
		"file_size":      len(data),
		"total_lines":    len(allLines),
		"total_matches":  totalMatches,
		"matches":        matches,
		"truncated":      totalMatches > maxMatches,
	}
	return toolEnvelope(s, payload)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// urlEscape percent-encodes a string for use as a query parameter value.
func urlEscape(s string) string {
	const hex = "0123456789ABCDEF"
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			out = append(out, c)
			continue
		}
		out = append(out, '%', hex[c>>4], hex[c&0x0f])
	}
	return string(out)
}

// urlEscapePath percent-encodes a path while preserving "/" boundaries.
func urlEscapePath(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = urlEscape(p)
	}
	return strings.Join(parts, "/")
}

// ---------- API call ----------

const responseFormatFooter = `## Response Format

When you have completed your investigation, respond with a single JSON object
matching this schema:

{
  "summary":        "1-2 sentence headline derived from root_cause",
  "is_transient":   true | false,
  "root_cause":     "the specific error found in evidence, with quoted log lines and artifact paths",
  "severity":       "Critical" | "High" | "Medium" | "Low",
  "suggested_fix":  "exact fix with file paths and changes needed",
  "relevant_files": ["path/relative/to/build/root1", "path2"]
}

Set is_transient=true only when the root cause is a known transient infra
issue (throttling, quota exhaustion, intermittent DNS, image-pull backoff,
disk pressure, etcd leader election) rather than a real bug in the code
under test. When in doubt, set is_transient=false.`

const systemPromptHeader = `You are an expert E2E test failure analyst for a Kubernetes Cluster API
project run on Prow. Each test run is a directory in GCS with a layout like:

- build-log.txt: top-level test-runner stdout/stderr. This is often hundreds
  of MB; never call read_artifact or tail_artifact for the full file. Use
  grep_artifact to find specific errors (e.g. pattern "(?i)error|fail").
- started.json, finished.json: small metadata files. Cheap to read directly.
- artifacts/ginkgo-log.txt: ginkgo run log; usually smaller than build-log.
- artifacts/junit*.xml: per-test results. Failing test bodies are here.
- artifacts/clusters/<cluster>/...: per-test cluster dumps (resources/, machines/, etc.)
- artifacts/clusters/<cluster>/machines/<vm>/: per-machine logs (kubelet.log, containerd.log, cloud-init-output.log, ...)

You have four tools for browsing artifacts:
  list_artifacts(path)                    - list immediate children of a dir
  read_artifact(path, offset, length)     - read byte range of a file (max 16KB)
  tail_artifact(path, lines)              - last N lines of a file (max 2000)
  grep_artifact(path, pattern, ...)       - RE2 regex search with line numbers

Strategy:
1. The user gives you the failing test name and excerpt. Identify which
   cluster/component owns the failure (e.g. KubeadmControlPlane, Machine,
   MachineDeployment, infrastructure provider).
2. Use list_artifacts to find the right cluster dump under artifacts/clusters/.
3. Read status of the relevant resource (status:) to find error conditions.
4. If a controller or machine is implicated, fetch its log (tail_artifact or
   grep_artifact).
5. Quote the actual error lines you find, with artifact paths and line numbers.
6. Do not speculate. If evidence is incomplete, say so explicitly.

Be efficient. You have a limited tool-call budget; prefer batches of focused
tool calls over many small ones. After 3-5 tools you should have enough
evidence to commit to a root cause — produce the final JSON answer rather
than chasing perfect completeness. A confident "best evidence I found" answer
beats running out of budget mid-investigation.

Watch the remaining_model_bytes and remaining_gcs_bytes returned with each
tool result; stop browsing and produce the final answer before they hit zero.`

const forceFinalizePrompt = `You have used your tool-call budget. Stop calling
tools. Produce the final JSON analysis now using the evidence you have already
gathered. If you did not find a definitive root cause, say so explicitly in
root_cause (e.g. "Investigation reached budget; best-evidence hypothesis is
X based on Y") rather than continuing to investigate.`

func runAgent(ctx context.Context, cfg *config) error {
	token := os.Getenv("AI_TOKEN")
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token == "" {
		return fmt.Errorf("AI_TOKEN or GITHUB_TOKEN required")
	}

	s := newState(cfg)

	sysPrompt := systemPromptHeader + "\n\n" + responseFormatFooter
	userPrompt := fmt.Sprintf("Build `%s/%s` (bucket %s) failed.\n",
		cfg.Job, cfg.Build, cfg.Bucket)
	if cfg.TestName != "" {
		userPrompt += fmt.Sprintf("\nFailing test:\n%s\n", cfg.TestName)
	}
	if cfg.FailureMessage != "" {
		userPrompt += fmt.Sprintf("\nFailure message excerpt:\n%s\n", cfg.FailureMessage)
	}
	userPrompt += "\nInvestigate the root cause using the artifact tools, then produce the JSON analysis."

	messages := []chatMessage{
		{Role: "system", Content: strPtr(sysPrompt)},
		{Role: "user", Content: strPtr(userPrompt)},
	}

	tools := toolDefs()
	deadline := s.startTime.Add(cfg.WallClock)
	loopCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	for iter := 0; iter < cfg.MaxIters; iter++ {
		s.roundTrips++
		log.Printf("--- iter %d (elapsed %s) ---", iter+1, time.Since(s.startTime).Truncate(time.Second))

		resp, err := callChat(loopCtx, s.httpClient, cfg.Endpoint, token, cfg.Model, messages, tools)
		if err != nil {
			return fmt.Errorf("iter %d: %w", iter+1, err)
		}
		s.promptTokens += resp.Usage.PromptTokens
		s.complTokens += resp.Usage.CompletionTokens

		if len(resp.Choices) == 0 {
			return fmt.Errorf("empty choices")
		}
		choice := resp.Choices[0]
		msg := choice.Message

		if len(msg.ToolCalls) == 0 {
			final := ""
			if msg.Content != nil {
				final = *msg.Content
			}
			printSummary(s, final, "stop")
			return nil
		}

		// Log + echo back the assistant message including tool_calls.
		if cfg.Verbose && msg.Content != nil && *msg.Content != "" {
			log.Printf("  assistant content: %s", truncate(*msg.Content, 200))
		}
		echo := chatMessage{Role: "assistant", ToolCalls: msg.ToolCalls}
		if msg.Content != nil {
			echo.Content = msg.Content
		}
		messages = append(messages, echo)

		// Execute every tool call in the response.
		for _, tc := range msg.ToolCalls {
			log.Printf("  tool: %s(%s)", tc.Function.Name, truncate(tc.Function.Arguments, 180))
			result := dispatchTool(loopCtx, s, tc)
			if cfg.Verbose {
				log.Printf("    => %s", truncate(result, 240))
			}
			messages = append(messages, chatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    strPtr(result),
			})
		}
	}

	// Out of tool-call budget. Force a finalization round with tools omitted
	// and an explicit user message asking for the JSON answer. This ensures we
	// always produce an A/B-comparable output, even if the model would otherwise
	// keep exploring forever.
	log.Printf("--- finalize (max iters reached, asking for JSON) ---")
	s.roundTrips++
	messages = append(messages, chatMessage{Role: "user", Content: strPtr(forceFinalizePrompt)})
	resp, err := callChat(loopCtx, s.httpClient, cfg.Endpoint, token, cfg.Model, messages, nil)
	if err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	s.promptTokens += resp.Usage.PromptTokens
	s.complTokens += resp.Usage.CompletionTokens
	if len(resp.Choices) > 0 && resp.Choices[0].Message.Content != nil {
		printSummary(s, *resp.Choices[0].Message.Content, "max_iters_finalize")
		return nil
	}
	printSummary(s, "(no final content from finalize round)", "max_iters_no_final")
	return fmt.Errorf("max iterations (%d) reached and finalize produced no content", cfg.MaxIters)
}

func callChat(ctx context.Context, httpClient *http.Client, endpoint, token, model string, messages []chatMessage, tools []toolDef) (*chatResponse, error) {
	body, err := json.Marshal(chatRequest{Model: model, Messages: messages, Tools: tools})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if strings.Contains(endpoint, "githubcopilot.com") {
		req.Header.Set("Copilot-Integration-Id", "copilot-developer-cli")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("chat returned %d: %s", resp.StatusCode, truncate(string(rb), 500))
	}
	var out chatResponse
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w; body=%s", err, truncate(string(rb), 500))
	}
	return &out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------- Reporting ----------

func printSummary(s *agentState, final, reason string) {
	elapsed := time.Since(s.startTime).Truncate(time.Millisecond)
	fmt.Println()
	fmt.Println("====================== SPIKE SUMMARY ======================")
	fmt.Printf("Stop reason:       %s\n", reason)
	fmt.Printf("Wall clock:        %s\n", elapsed)
	fmt.Printf("Round trips:       %d\n", s.roundTrips)
	fmt.Printf("Prompt tokens:     %d\n", s.promptTokens)
	fmt.Printf("Completion tokens: %d\n", s.complTokens)
	fmt.Printf("Total tokens:      %d\n", s.promptTokens+s.complTokens)
	fmt.Printf("Model bytes used:  %d / %d\n", s.modelBytes, s.cfg.ModelByteBudget)
	fmt.Printf("GCS bytes fetched: %d / %d\n", s.gcsBytes, s.cfg.GCSByteBudget)
	fmt.Printf("Files cached:      %d\n", len(s.cache))
	fmt.Println("Tool call counts:")
	for name, n := range s.toolCallsBy {
		fmt.Printf("  %-16s %d\n", name, n)
	}
	fmt.Println("===========================================================")
	fmt.Println("Final answer:")
	fmt.Println(final)
}

// ---------- main ----------

func main() {
	cfg := parseFlags()
	if err := runAgent(context.Background(), cfg); err != nil {
		log.Fatalf("spike failed: %v", err)
	}
}
