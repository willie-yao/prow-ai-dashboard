package ai

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

const analysisTraceMaxFileBytes = 64 << 20

// LoadTraceStore loads an existing private trace snapshot. Missing files return
// an empty store so polling and webhook writers can share one upsert path.
func LoadTraceStore(path string) (*TraceStore, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewTraceStore(), nil
		}
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, analysisTraceMaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > analysisTraceMaxFileBytes {
		return nil, fmt.Errorf("trace file exceeds %d bytes", analysisTraceMaxFileBytes)
	}
	var snapshot AnalysisTraceFile
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("decode trace file: %w", err)
	}
	if snapshot.Version > analysisTraceVersion {
		return nil, fmt.Errorf("trace version %d is newer than supported version %d", snapshot.Version, analysisTraceVersion)
	}
	store := NewTraceStore()
	store.dropped = snapshot.DroppedTraces
	for _, trace := range snapshot.Traces {
		store.Upsert(trace)
	}
	return store, nil
}

// HasTask reports whether an Orka Task already has a persisted trace.
func (s *TraceStore) HasTask(namespace, taskName string) bool {
	if s == nil || strings.TrimSpace(taskName) == "" {
		return false
	}
	namespace = traceText(namespace)
	taskName = traceText(taskName)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, trace := range s.traces {
		if trace.Backend == "orka" && trace.TaskNamespace == namespace && trace.TaskName == taskName {
			return true
		}
	}
	return false
}

// Full reports whether the store cannot accept another distinct trace.
func (s *TraceStore) Full() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.traces) >= analysisTraceMaxTraces
}

// Upsert adds or replaces one completed trace using its backend identity.
func (s *TraceStore) Upsert(trace AnalysisTrace) bool {
	if s == nil {
		return false
	}
	trace = normalizeAnalysisTrace(trace)
	key := analysisTraceIdentity(trace)
	if key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.traces {
		if analysisTraceIdentity(s.traces[i]) == key {
			s.traces[i] = trace
			return true
		}
	}
	if len(s.traces) >= analysisTraceMaxTraces {
		s.dropped++
		return false
	}
	s.traces = append(s.traces, trace)
	return true
}

func analysisTraceIdentity(trace AnalysisTrace) string {
	if trace.Backend == "orka" && trace.TaskName != "" {
		return "orka\x00" + trace.TaskNamespace + "\x00" + trace.TaskName
	}
	if trace.JobID == "" && trace.BuildID == "" && trace.TestName == "" {
		return ""
	}
	return trace.Backend + "\x00" + trace.JobID + "\x00" + trace.BuildID + "\x00" + trace.TestName
}

func normalizeAnalysisTrace(trace AnalysisTrace) AnalysisTrace {
	trace.Backend = traceText(trace.Backend)
	if trace.Backend == "" {
		trace.Backend = "inprocess"
	}
	trace.TaskName = traceText(trace.TaskName)
	trace.TaskNamespace = traceText(trace.TaskNamespace)
	trace.ContractHash = traceText(trace.ContractHash)
	trace.JobID = traceText(trace.JobID)
	trace.BuildID = traceText(trace.BuildID)
	trace.TestName = traceText(trace.TestName)
	trace.APIMode = traceText(trace.APIMode)
	trace.StartedAt = traceText(trace.StartedAt)
	trace.Outcome = traceCode(trace.Outcome)
	if trace.ErrorCode != "" {
		trace.ErrorCode = traceCode(trace.ErrorCode)
	}
	if trace.ElapsedMs < 0 {
		trace.ElapsedMs = 0
	}
	if len(trace.Events) > analysisTraceMaxEvents {
		trace.Events = trace.Events[:analysisTraceMaxEvents]
		trace.Truncated = true
	}
	for i := range trace.Events {
		event := &trace.Events[i]
		event.Sequence = i + 1
		if event.ElapsedMs < 0 {
			event.ElapsedMs = 0
		}
		event.Kind = traceText(event.Kind)
		event.Outcome = traceText(event.Outcome)
		event.ResponseID = traceResponseID(event.ResponseID)
		event.Status = traceText(event.Status)
		event.FinishReason = traceText(event.FinishReason)
		event.Tool = traceText(event.Tool)
		if event.ErrorCode != "" {
			event.ErrorCode = traceCode(event.ErrorCode)
		}
	}
	if trace.Events == nil {
		trace.Events = []TraceEvent{}
	}
	return trace
}
