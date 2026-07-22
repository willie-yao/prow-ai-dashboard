package ai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTraceStoreBoundsAndRedacts(t *testing.T) {
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIResponses})
	for i := 0; i < analysisTraceMaxEvents+2; i++ {
		trace.Record(TraceEvent{Kind: "model_request", Error: "failed https://secret.example/v1?token=hidden Authorization: Bearer top-secret " + strings.Repeat("x", analysisTraceMaxText)})
	}
	trace.Finish("error", nil)

	snapshot := store.Snapshot()
	if len(snapshot.Traces) != 1 {
		t.Fatalf("traces = %d, want 1", len(snapshot.Traces))
	}
	got := snapshot.Traces[0]
	if !got.Truncated || len(got.Events) != analysisTraceMaxEvents {
		t.Fatalf("trace bounds = truncated:%v events:%d", got.Truncated, len(got.Events))
	}
	if strings.Contains(got.Events[0].Error, "secret.example") || !strings.Contains(got.Events[0].Error, "[redacted-url]") {
		t.Fatalf("error was not redacted: %q", got.Events[0].Error)
	}
	if strings.Contains(got.Events[0].Error, "top-secret") {
		t.Fatalf("credential was not redacted: %q", got.Events[0].Error)
	}
	if len(got.Events[0].Error) > analysisTraceMaxText+3 {
		t.Fatalf("error length = %d", len(got.Events[0].Error))
	}
}

func TestTraceStoreSaveUsesPrivateSchema(t *testing.T) {
	store := NewTraceStore()
	second := store.Start(TraceMetadata{JobID: "job-b", BuildID: "2", TestName: "test-b", APIMode: APIChatCompletions})
	second.Record(TraceEvent{Kind: "tool_call", Tool: "read_artifact", Bytes: 42})
	second.Finish("success", nil)
	first := store.Start(TraceMetadata{JobID: "job-a", BuildID: "1", TestName: "test-a", APIMode: APIResponses})
	first.Finish("cache_hit", nil)
	store.mu.Lock()
	for i := range store.traces {
		store.traces[i].StartedAt = "2026-07-22T00:00:00Z"
	}
	store.mu.Unlock()

	path := filepath.Join(t.TempDir(), "ai_traces.json")
	if err := store.Save(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got AnalysisTraceFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != analysisTraceVersion || len(got.Traces) != 2 {
		t.Fatalf("snapshot = %+v", got)
	}
	if got.Traces[0].JobID != "job-a" || got.Traces[1].JobID != "job-b" {
		t.Fatalf("trace order = %+v", got.Traces)
	}
	if strings.Contains(string(data), "endpoint") || strings.Contains(string(data), "model") {
		t.Fatalf("trace leaked provider configuration: %s", data)
	}
}

func TestTraceStoreCapsCompletedTraces(t *testing.T) {
	store := NewTraceStore()
	for i := 0; i < analysisTraceMaxTraces+2; i++ {
		trace := store.Start(TraceMetadata{JobID: "job"})
		trace.Finish("success", nil)
	}
	got := store.Snapshot()
	if len(got.Traces) != analysisTraceMaxTraces || got.DroppedTraces != 2 {
		t.Fatalf("traces=%d dropped=%d", len(got.Traces), got.DroppedTraces)
	}
}
