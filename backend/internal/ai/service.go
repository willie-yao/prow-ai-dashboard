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
// Behavior matches the pre-refactor analyzeFailuresWithAI loop:
//   - skip entirely if both AISummary and AIAnalysis are already populated
//   - if the module flags the failure as a known transient, set AISummary with
//     the reason (IsTransient=true) and return without running the API
//   - otherwise run a quick summary, then a deep analysis; errors at either
//     step are logged but the partial state is preserved
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

	if tc.AISummary == nil {
		summary, err := s.client.doQuickSummary(ctx,
			s.quickCacheKey(tc.Name, tc.FailureMessage),
			s.module.SystemPrompt(),
			s.module.QuickSummaryPrompt(tc.Name, tc.FailureMessage, tc.FailureLocation),
		)
		if err != nil {
			log.Printf("  ⚠ AI summary failed for %s: %v", tc.Name, err)
			return
		}
		tc.AISummary = summary
	}

	log.Printf("  🔍 Deep analyzing: %s", tc.Name)
	consec := s.consecutiveMap[tc.Name]
	if consec < 1 {
		consec = 1
	}

	userPrompt := s.module.DeepAnalysisPrompt(ctx, httpClient, run, tc, consec)
	analysis, err := s.client.doDeepAnalysis(ctx,
		s.deepCacheKey(tc.Name, tc.FailureMessage),
		s.module.SystemPrompt(),
		userPrompt,
	)
	if err != nil {
		log.Printf("  ⚠ AI deep analysis failed for %s: %v", tc.Name, err)
		return
	}
	tc.AIAnalysis = analysis
}

// quickCacheKey returns the cache key for a quick summary.
// For module "capi" we keep the legacy "summary:<hash>" shape so existing
// CAPZ caches stay valid. New modules get a "quick:<module>:<hash>" key.
func (s *Service) quickCacheKey(testName, failureMessage string) string {
	hash := failureHash(testName, failureMessage)
	if s.module.Name() == "capi" {
		return fmt.Sprintf("summary:%x", hash)
	}
	return fmt.Sprintf("quick:%s:%x", s.module.Name(), hash)
}

// deepCacheKey returns the cache key for a deep/comprehensive analysis.
// Legacy "comprehensive:<hash>" for capi; "deep:<module>:<hash>" otherwise.
func (s *Service) deepCacheKey(testName, failureMessage string) string {
	hash := failureHash(testName, failureMessage)
	if s.module.Name() == "capi" {
		return fmt.Sprintf("comprehensive:%x", hash)
	}
	return fmt.Sprintf("deep:%s:%x", s.module.Name(), hash)
}

// failureHash builds the deterministic hash used by both cache key flavors.
// Matches the original cacheKey() / comprehensiveCacheKey() output byte-for-byte
// so legacy CAPZ caches keep hitting.
func failureHash(testName, failureMessage string) []byte {
	normalized := normalizeError(failureMessage)
	h := sha256.Sum256([]byte(testName + normalized))
	return h[:8]
}
