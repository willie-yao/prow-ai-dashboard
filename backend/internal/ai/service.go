package ai

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// Service orchestrates AI analysis for a single project. It composes a generic
// API Client with a project-specific Module and a snapshot of consecutive
// failure counts.
type Service struct {
	client         *Client
	module         Module
	consecutiveMap map[string]int
}

// NewService constructs a Service. consecutiveMap is a lookup of test name to
// consecutive failure count taken from the flakiness report; it may be nil.
func NewService(client *Client, module Module, consecutiveMap map[string]int) *Service {
	if consecutiveMap == nil {
		consecutiveMap = map[string]int{}
	}
	return &Service{
		client:         client,
		module:         module,
		consecutiveMap: consecutiveMap,
	}
}

// Module returns the underlying Module (mainly for logging).
func (s *Service) Module() Module { return s.module }

// Analyze fills tc.AISummary and tc.AIAnalysis for a single failed test case.
// Behavior:
//   - skip entirely if both AISummary and AIAnalysis are already populated
//   - if the module flags the failure as a known transient, set AISummary with
//     the reason (IsTransient=true) and return without running the API
//   - otherwise run a single combined chat completion that produces both the
//     headline summary and the deep root-cause fields from the same evidence
//   - on API failure, leave an "AI analysis unavailable" summary so the UI
//     still has something to render
func (s *Service) Analyze(ctx context.Context, httpClient *http.Client, run *models.BuildResult, tc *models.TestCase) {
	if tc.AISummary != nil && tc.AIAnalysis != nil {
		return
	}

	if reason := s.module.IsKnownTransient(tc.FailureMessage); reason != "" {
		tc.AISummary = &models.AISummary{
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			Summary:     reason,
			IsTransient: true,
		}
		log.Printf("  ⏭ Skipping transient: %s: %s", tc.Name, reason)
		return
	}

	log.Printf("  🔍 Analyzing: %s", tc.Name)
	consec := s.consecutiveMap[tc.Name]
	if consec < 1 {
		consec = 1
	}

	userPrompt := s.module.AnalysisPrompt(ctx, httpClient, run, tc, consec)
	summary, analysis, err := s.client.doAnalyze(ctx,
		s.cacheKey(tc.Name, tc.FailureMessage),
		s.module.SystemPrompt(),
		userPrompt,
	)
	if err != nil {
		log.Printf("  ⚠ AI analysis failed for %s: %v", tc.Name, err)
		if tc.AISummary == nil {
			tc.AISummary = &models.AISummary{
				GeneratedAt: time.Now().UTC().Format(time.RFC3339),
				Summary:     "AI analysis unavailable: " + err.Error(),
				IsTransient: false,
			}
		}
		return
	}
	tc.AISummary = summary
	tc.AIAnalysis = analysis
}

// cacheKey returns the cache key for the combined analysis call. For the
// "capi" module we keep the legacy "comprehensive:<hash>" shape so existing
// CAPZ deep-analysis caches stay valid (the on-disk payload shape changed,
// but stale entries are tolerated by the doAnalyze unmarshal fallback). New
// modules get an "analyze:<module>:<hash>" key.
func (s *Service) cacheKey(testName, failureMessage string) string {
	hash := failureHash(testName, failureMessage)
	if s.module.Name() == "capi" {
		return fmt.Sprintf("comprehensive:%x", hash)
	}
	return fmt.Sprintf("analyze:%s:%x", s.module.Name(), hash)
}

// failureHash builds the deterministic hash used by both cache key flavors.
// Matches the original cacheKey() / comprehensiveCacheKey() output byte-for-byte
// so legacy CAPZ caches keep hitting.
func failureHash(testName, failureMessage string) []byte {
	normalized := normalizeError(failureMessage)
	h := sha256.Sum256([]byte(testName + normalized))
	return h[:8]
}
