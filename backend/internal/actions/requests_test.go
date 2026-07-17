package actions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

func requestTestService(t *testing.T) (*Service, models.PatternAnalysis) {
	t.Helper()
	dataDir := t.TempDir()
	pattern := systemicPattern()
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{
		JobID: "periodic-x", PatternAnalyses: []models.PatternAnalysis{pattern},
	})
	cfg := &project.Config{
		Branding: project.Branding{SiteURL: "https://dash.example.com", SourceRepo: project.SourceRepo{Owner: "o", Name: "r"}},
		Issues:   &project.Issues{Repo: &project.SourceRepo{Owner: "o", Name: "r"}},
	}
	return NewService(cfg, dataDir, AIConfig{}), pattern
}

func waitRequest(t *testing.T, service *Service, id, owner string, want ...string) ActionRequestView {
	t.Helper()
	allowed := map[string]bool{}
	for _, status := range want {
		allowed[status] = true
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, err := service.GetRequest(id, owner)
		if err == nil && allowed[view.Status] {
			return view
		}
		time.Sleep(10 * time.Millisecond)
	}
	view, err := service.GetRequest(id, owner)
	t.Fatalf("request did not reach %v: view=%+v err=%v", want, view, err)
	return ActionRequestView{}
}

func TestAsyncIssueRequestPersistsAndNotifies(t *testing.T) {
	service, pattern := requestTestService(t)
	notified := make(chan ActionRequestView, 1)
	service.ConfigureAsyncRequests(time.Minute, func(_ context.Context, view ActionRequestView) error {
		notified <- view
		return nil
	})

	created, err := service.CreateRequest(pattern.ID, "create-issue", "Alice", "token", "")
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != RequestPending || created.Owner != "alice" {
		t.Fatalf("created = %+v", created)
	}
	ready := waitRequest(t, service, created.ID, "alice", RequestReady)
	if ready.Preview == nil || ready.Preview.Kind != "issue" || ready.Preview.Title == "" {
		t.Fatalf("ready = %+v", ready)
	}
	select {
	case got := <-notified:
		if got.ID != created.ID || got.Status != RequestReady {
			t.Fatalf("notification = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("draft-ready notifier was not called")
	}
	ready = waitRequest(t, service, created.ID, "alice", RequestReady)
	if !ready.EmailSent {
		t.Fatalf("email status not persisted: %+v", ready)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	persisted, err := reloaded.GetRequest(created.ID, "alice")
	if err != nil || persisted.Status != RequestReady || persisted.Preview == nil {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
	if _, err := reloaded.GetRequest(created.ID, "bob"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("cross-owner lookup err=%v", err)
	}
}

func TestPendingRequestBecomesFailedAfterRestart(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	state := actionRequestState{Version: 1, Requests: map[string]*actionRequest{
		"request-1": {ActionRequestView: ActionRequestView{
			ID: "request-1", Owner: "alice", Status: RequestPending,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
		}},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	view, err := reloaded.GetRequest("request-1", "alice")
	if err != nil || view.Status != RequestFailed || view.Error == "" {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestCancelReadyRequest(t *testing.T) {
	service, pattern := requestTestService(t)
	created, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, created.ID, "alice", RequestReady)
	if err := service.CancelRequest(created.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	view, err := service.GetRequest(created.ID, "alice")
	if err != nil || view.Status != RequestCancelled {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestConfigureAsyncRequestsRetriesPersistedReadyEmail(t *testing.T) {
	service, _ := requestTestService(t)
	now := time.Now().UTC()
	state := actionRequestState{Version: 1, Requests: map[string]*actionRequest{
		"request-ready": {ActionRequestView: ActionRequestView{
			ID: "request-ready", Owner: "alice", Kind: "create-issue", Status: RequestReady,
			CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339),
			ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
			Preview:   &PreviewResult{Kind: "issue", Title: "Ready", Body: "Body"},
		}},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	notified := make(chan ActionRequestView, 1)
	reloaded.ConfigureAsyncRequests(time.Minute, func(_ context.Context, view ActionRequestView) error {
		notified <- view
		return nil
	})
	select {
	case view := <-notified:
		if view.ID != "request-ready" {
			t.Fatalf("notification = %+v", view)
		}
	case <-time.After(time.Second):
		t.Fatal("persisted ready request was not retried")
	}
	view := waitRequest(t, reloaded, "request-ready", "alice", RequestReady)
	deadline := time.Now().Add(time.Second)
	for !view.EmailSent && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		view = waitRequest(t, reloaded, "request-ready", "alice", RequestReady)
	}
	if !view.EmailSent {
		t.Fatalf("email status = %+v", view)
	}
}
