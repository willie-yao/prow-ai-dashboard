// Command orka-ingestor patches Orka analysis results back into the fetcher's
// dashboard skeleton. For every failing test in jobs/*.json it re-derives the
// producer's content-addressed Task name, fetches that Task's result from the
// Orka API, parses the analysis JSON, and writes ai_summary/ai_analysis onto the
// test case. The frontend then renders a dashboard produced entirely by Orka.
//
// Idempotent: re-running patches whatever results are now available and leaves
// the rest untouched, so it can run repeatedly as Tasks complete.
//
// TEMPORARY: lives only on the `orka` branch alongside experimental/orka/.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orkamig"
)

func main() {
	dataDir := flag.String("data", "data", "dashboard skeleton dir to patch in place (holds jobs/*.json)")
	apiBase := flag.String("api", "http://localhost:8080", "Orka API base URL")
	token := flag.String("token", "", "bearer token for the Orka API (or set -token-file)")
	tokenFile := flag.String("token-file", "", "file holding the bearer token")
	version := flag.String("version", "v1", "content-address version suffix (must match the producer run)")
	model := flag.String("model", "claude-sonnet-4.5", "model label recorded on each analysis")
	wait := flag.Duration("wait", 0, "keep polling until every failing test is patched or this deadline elapses (0 = single pass)")
	poll := flag.Duration("poll", 15*time.Second, "interval between passes when -wait is set")
	flag.Parse()

	tok := strings.TrimSpace(*token)
	if tok == "" && *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			log.Fatalf("read token-file: %v", err)
		}
		tok = strings.TrimSpace(string(b))
	}
	if tok == "" {
		// In-cluster default: the pod's ServiceAccount token (TokenReview accepts it).
		if b, err := os.ReadFile(saTokenPath); err == nil {
			tok = strings.TrimSpace(string(b))
		}
	}

	client := &orkaClient{base: strings.TrimRight(*apiBase, "/"), token: tok, http: &http.Client{Timeout: 30 * time.Second}}

	deadline := time.Now().Add(*wait)
	for {
		patched, failedTests, missing := ingestPass(client, *dataDir, *version, *model)
		log.Printf("patched %d/%d failing tests (%d results missing/unavailable)", patched, failedTests, missing)
		if *wait <= 0 || missing == 0 || time.Now().After(deadline) {
			return
		}
		time.Sleep(*poll)
	}
}

// saTokenPath is the standard in-cluster ServiceAccount token mount.
const saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// ingestPass patches every available result into the skeleton once, returning
// how many failing tests it patched, saw, and could not yet resolve.
func ingestPass(client *orkaClient, dataDir, version, model string) (patched, failedTests, missing int) {
	jobFiles, _ := filepath.Glob(filepath.Join(dataDir, "jobs", "*.json"))
	for _, jf := range jobFiles {
		raw, err := os.ReadFile(jf)
		if err != nil {
			continue
		}
		var detail models.JobDetail
		if json.Unmarshal(raw, &detail) != nil {
			continue
		}
		changed := false
		for ri := range detail.Runs {
			run := &detail.Runs[ri]
			for ti := range run.TestCases {
				tc := &run.TestCases[ti]
				if tc.Status != "failed" {
					continue
				}
				failedTests++
				if tc.AIAnalysis != nil {
					continue // already patched by an earlier pass
				}
				name := orkamig.TaskName(run.BuildID, orkamig.FailureHash(tc.Name, tc.FailureMessage), version)
				result, ok := client.result(name)
				if !ok {
					missing++
					continue
				}
				a, ok := parseAnalysis(result)
				if !ok {
					missing++
					continue
				}
				now := time.Now().UTC().Format(time.RFC3339)
				tc.AISummary = &models.AISummary{GeneratedAt: now, Summary: a.RootCause, IsTransient: a.IsTransient}
				tc.AIAnalysis = &models.AIAnalysis{
					GeneratedAt:   now,
					Model:         model,
					RootCause:     a.RootCause,
					Severity:      a.Severity,
					SuggestedFix:  a.SuggestedFix,
					RelevantFiles: a.RelevantFiles,
					Mode:          "agentic",
				}
				patched++
				changed = true
			}
		}
		if changed {
			out, err := json.MarshalIndent(detail, "", "  ")
			if err != nil {
				log.Printf("marshal %s: %v", jf, err)
				continue
			}
			if err := os.WriteFile(jf, out, 0o644); err != nil {
				log.Printf("write %s: %v", jf, err)
			}
		}
	}
	return patched, failedTests, missing
}

// analysis is the model's output JSON shape.
type analysis struct {
	RootCause     string   `json:"root_cause"`
	Severity      string   `json:"severity"`
	IsTransient   bool     `json:"is_transient"`
	SuggestedFix  string   `json:"suggested_fix"`
	RelevantFiles []string `json:"relevant_files"`
}

// parseAnalysis extracts the last balanced JSON object containing an analysis
// from the model's (possibly prose-wrapped) result text.
func parseAnalysis(text string) (analysis, bool) {
	var best analysis
	found := false
	depth, start := 0, -1
	for i, ch := range text {
		switch ch {
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					var a analysis
					if json.Unmarshal([]byte(text[start:i+1]), &a) == nil && (a.RootCause != "" || a.Severity != "") {
						best, found = a, true
					}
				}
			}
		}
	}
	return best, found
}

type orkaClient struct {
	base  string
	token string
	http  *http.Client
}

// result fetches a Task's result text, or ok=false if not available.
func (c *orkaClient) result(taskName string) (string, bool) {
	req, err := http.NewRequest(http.MethodGet, c.base+"/api/v1/tasks/"+taskName+"/result", nil)
	if err != nil {
		return "", false
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", false
	}
	var wrap struct {
		Result string `json:"result"`
	}
	if json.Unmarshal(body, &wrap) != nil || wrap.Result == "" {
		return "", false
	}
	return wrap.Result, true
}
