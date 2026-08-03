package aiusage

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testRecorder(t *testing.T, path string, now time.Time, recent int) *Recorder {
	t.Helper()
	pricing, err := NewPriceTable(Rates{Currency: "USD", InputPerMillion: "1", CachedInputPerMillion: "0.5", OutputPerMillion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := NewRecorder(path, RecorderOptions{
		RetentionDays: 2, RecentOperations: recent, Pricing: pricing, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return recorder
}

func TestOperationRecordsProviderUsage(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 10)
	ctx, operation := Begin(t.Context(), recorder, Metadata{ID: "0011223344556677", Origin: OriginFetcher, Feature: FeatureFailureAnalysis, StartedAt: now})
	ObserveModelRequest(ctx, TokenUsage{Reported: true, InputTokens: 100, CachedInputTokens: 20, OutputTokens: 10, ReasoningTokens: 4})
	ObserveModelRequest(ctx, TokenUsage{})
	got := operation.Finish(OutcomeSuccess)
	if got.ModelRequests != 2 || got.ReportedRequests != 1 || got.UnreportedRequests != 1 {
		t.Fatalf("request coverage = %+v", got)
	}
	if got.InputTokens != 100 || got.CachedInputTokens != 20 || got.OutputTokens != 10 || got.ReasoningTokens != 4 {
		t.Fatalf("tokens = %+v", got)
	}
	if got.EstimatedCostNanos == 0 || got.PricingHash == "" {
		t.Fatalf("pricing = %+v", got)
	}
	snapshot := recorder.Snapshot()
	if len(snapshot.Days) != 1 || snapshot.Days[0].Totals.ModelRequests != 2 || len(snapshot.RecentOperations) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestOperationFinishIsIdempotent(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 10)
	ctx, operation := Begin(t.Context(), recorder, Metadata{ID: "0011223344556677", Origin: OriginServer, Feature: FeatureAnalysisChat, StartedAt: now})
	ObserveModelRequest(ctx, TokenUsage{Reported: true, InputTokens: 10})
	operation.Finish(OutcomeSuccess)
	operation.Finish(OutcomeSuccess)
	snapshot := recorder.Snapshot()
	if snapshot.Days[0].Totals.Operations != 1 || snapshot.Days[0].Totals.InputTokens != 10 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestRecorderUpsertsByID(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 10)
	completed := now.Format(time.RFC3339Nano)
	recorder.Record(OperationUsage{ID: "0011223344556677", Origin: OriginAnalyzer, Feature: FeatureFailureAnalysis, StartedAt: completed, CompletedAt: completed, Outcome: OutcomeSuccess, ModelRequests: 1, ReportedRequests: 1, InputTokens: 10})
	recorder.Record(OperationUsage{ID: "0011223344556677", Origin: OriginAnalyzer, Feature: FeatureFailureAnalysis, StartedAt: completed, CompletedAt: completed, Outcome: OutcomeSuccess, ModelRequests: 1, ReportedRequests: 1, InputTokens: 20})
	snapshot := recorder.Snapshot()
	if snapshot.Days[0].Totals.Operations != 1 || snapshot.Days[0].Totals.InputTokens != 20 || len(snapshot.RecentOperations) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestRecorderPrunesRetentionAndRecentOperations(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 1)
	ids := []string{"0000000000000001", "0000000000000002", "0000000000000003"}
	for i, completed := range []time.Time{now.AddDate(0, 0, -2), now.AddDate(0, 0, -1), now} {
		stamp := completed.Format(time.RFC3339Nano)
		recorder.Record(OperationUsage{ID: ids[i], Origin: OriginFetcher, Feature: FeaturePatternAnalysis, StartedAt: stamp, CompletedAt: stamp, Outcome: OutcomeSuccess})
	}
	snapshot := recorder.Snapshot()
	if len(snapshot.Days) != 2 || snapshot.Days[0].Date != "2026-08-02" || snapshot.Days[1].Date != "2026-08-03" {
		t.Fatalf("days = %+v", snapshot.Days)
	}
	if len(snapshot.RecentOperations) != 1 || snapshot.RecentOperations[0].ID != "0000000000000003" || snapshot.DroppedOperations != 1 {
		t.Fatalf("recent = %+v dropped=%d", snapshot.RecentOperations, snapshot.DroppedOperations)
	}
}

func TestRecorderCanDisableRecentOperations(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 0)
	stamp := now.Format(time.RFC3339Nano)
	recorder.Record(OperationUsage{ID: "0011223344556677", Origin: OriginFetcher, Feature: FeatureFailureAnalysis, StartedAt: stamp, CompletedAt: stamp, Outcome: OutcomeSuccess})
	snapshot := recorder.Snapshot()
	if len(snapshot.Days) != 1 || len(snapshot.RecentOperations) != 0 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestNewRecorderRejectsMalformedAndNewerLedgers(t *testing.T) {
	dir := t.TempDir()
	malformed := filepath.Join(dir, "malformed.json")
	if err := os.WriteFile(malformed, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRecorder(malformed, RecorderOptions{RetentionDays: 30, RecentOperations: 10}); err == nil {
		t.Fatal("expected malformed-ledger error")
	}
	newer := filepath.Join(dir, "newer.json")
	if err := os.WriteFile(newer, []byte(`{"version":2,"days":[],"recent_operations":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRecorder(newer, RecorderOptions{RetentionDays: 30, RecentOperations: 10}); err == nil {
		t.Fatal("expected newer-version error")
	}
}

func TestRecorderWritesPrivateFile(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "private", "usage.json")
	recorder := testRecorder(t, path, now, 10)
	stamp := now.Format(time.RFC3339Nano)
	recorder.Record(OperationUsage{ID: "0011223344556677", Origin: OriginFetcher, Feature: FeatureFailureAnalysis, StartedAt: stamp, CompletedAt: stamp, Outcome: OutcomeCacheHit})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	loaded, err := NewRecorder(path, RecorderOptions{RetentionDays: 2, RecentOperations: 10, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Snapshot().Days[0].Totals.CacheHits; got != 1 {
		t.Fatalf("cache hits = %d, want 1", got)
	}
}

func TestRecorderWriteFailureIsNonFatal(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	want := errors.New("write failed")
	var logs []string
	recorder, err := NewRecorder("usage.json", RecorderOptions{
		RetentionDays: 2, RecentOperations: 10, Now: func() time.Time { return now },
		Write: func(string, any) error { return want },
		Logf:  func(format string, _ ...any) { logs = append(logs, format) },
	})
	if err != nil {
		t.Fatal(err)
	}
	stamp := now.Format(time.RFC3339Nano)
	got := recorder.Record(OperationUsage{ID: "0011223344556677", Origin: OriginServer, Feature: FeatureAnalysisChat, StartedAt: stamp, CompletedAt: stamp, Outcome: OutcomeSuccess})
	if got.ID != "0011223344556677" || len(logs) != 1 || recorder.Snapshot().Days[0].Totals.Operations != 1 {
		t.Fatalf("operation=%+v logs=%v snapshot=%+v", got, logs, recorder.Snapshot())
	}
}

func TestRecorderConcurrentOperations(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := testRecorder(t, "", now, 100)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, operation := Begin(t.Context(), recorder, Metadata{Origin: OriginFetcher, Feature: FeatureFailureAnalysis, StartedAt: now})
			ObserveModelRequest(ctx, TokenUsage{Reported: true, InputTokens: 1})
			operation.Finish(OutcomeSuccess)
		}()
	}
	wg.Wait()
	snapshot := recorder.Snapshot()
	if snapshot.Days[0].Totals.Operations != 50 || snapshot.Days[0].Totals.InputTokens != 50 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}
