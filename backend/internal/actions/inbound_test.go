package actions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHandleEmailReplyCreatesDraftAndDeduplicates(t *testing.T) {
	service, pattern := requestTestService(t)
	result, err := service.HandleEmailReply(
		"<message-1@example.com>", "pattern", pattern.ID, "Alice", "read-token",
		"issue:\nMention the IPv6 impact.\n\nOn Friday someone wrote:\n> old message",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Duplicate || result.Request.Status != RequestPending || result.Request.Kind != "create-issue" || result.Request.Owner != "alice" {
		t.Fatalf("result = %+v", result)
	}
	ready := waitRequest(t, service, result.Request.ID, "alice", RequestReady)
	if ready.Preview == nil || ready.Preview.Kind != "issue" {
		t.Fatalf("ready = %+v", ready)
	}

	duplicate, err := service.HandleEmailReply(
		"<message-1@example.com>", "pattern", pattern.ID, "alice", "", "fix",
	)
	if err != nil || !duplicate.Duplicate || duplicate.Request.ID != result.Request.ID {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	data, err := os.ReadFile(filepath.Join(service.dataDir, "action_request_state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "message-1@example.com") {
		t.Fatal("raw inbound message id was persisted")
	}
	if strings.Contains(string(data), "read-token") {
		t.Fatal("inbound GitHub token was persisted")
	}
}

func TestHandleEmailReplyRevisesReadyDraft(t *testing.T) {
	service, pattern := requestTestService(t)
	created, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "Initial instruction")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, created.ID, "alice", RequestReady)

	result, err := service.HandleEmailReply(
		"<message-2@example.com>", "request", created.ID, "alice", "",
		"Explain why the timeout is safe.\n--\nSignature",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Duplicate || result.Request.ID != created.ID || result.Request.Status != RequestPending {
		t.Fatalf("result = %+v", result)
	}
	waitRequest(t, service, created.ID, "alice", RequestReady)
	service.rmu.Lock()
	instruction := service.requests.Requests[created.ID].Instruction
	service.rmu.Unlock()
	for _, want := range []string{"Initial instruction", "Additional email instructions", "Explain why the timeout is safe."} {
		if !strings.Contains(instruction, want) {
			t.Fatalf("instruction %q missing %q", instruction, want)
		}
	}
}

func TestHandleEmailReplyNeverConfirms(t *testing.T) {
	service, pattern := requestTestService(t)
	created, err := service.CreateRequest(pattern.ID, "create-issue", "alice", "token", "")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, created.ID, "alice", RequestReady)

	for _, body := range []string{"confirm", "approve and post it", "yes"} {
		if _, err := service.HandleEmailReply("<"+body+"@example.com>", "request", created.ID, "alice", "", body); err == nil || !strings.Contains(err.Error(), "cannot confirm or post") {
			t.Fatalf("body %q err=%v", body, err)
		}
	}
	view, err := service.GetRequest(created.ID, "alice")
	if err != nil || view.Status != RequestReady || view.ResultURL != "" {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestHandleEmailReplyRequiresExplicitPatternCommand(t *testing.T) {
	service, pattern := requestTestService(t)
	for i, body := range []string{"please investigate", "confirm", "> quoted only"} {
		messageID := "<invalid-" + string(rune('a'+i)) + "@example.com>"
		if _, err := service.HandleEmailReply(messageID, "pattern", pattern.ID, "alice", "", body); err == nil {
			t.Fatalf("body %q was accepted", body)
		}
	}
}

func TestHandleEmailReplyDedupSurvivesRestart(t *testing.T) {
	service, pattern := requestTestService(t)
	first, err := service.HandleEmailReply("<message-3@example.com>", "pattern", pattern.ID, "alice", "", "issue")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, first.Request.ID, "alice", RequestReady)

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	duplicate, err := reloaded.HandleEmailReply("<message-3@example.com>", "pattern", pattern.ID, "alice", "", "fix")
	if err != nil || !duplicate.Duplicate || duplicate.Request.ID != first.Request.ID {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
}

func TestPendingInboundRequestResumesAfterRestart(t *testing.T) {
	service, pattern := requestTestService(t)
	now := time.Now().UTC()
	receiptKey, err := emailMessageKey("<restart@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	state := actionRequestState{
		Version: 2,
		Requests: map[string]*actionRequest{
			"request-inbound": {ActionRequestView: ActionRequestView{
				ID: "request-inbound", FailureID: pattern.ID, Kind: "create-issue", Owner: "alice", Status: RequestPending,
				CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
			}},
		},
		Inbound: map[string]inboundReceipt{
			receiptKey: {RequestID: "request-inbound", ReceivedAt: now.Format(time.RFC3339)},
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(service.dataDir, "action_request_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	pending, err := reloaded.GetRequest("request-inbound", "alice")
	if err != nil || pending.Status != RequestPending {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	reloaded.ResumeInboundRequests("")
	ready := waitRequest(t, reloaded, "request-inbound", "alice", RequestReady)
	if ready.Preview == nil || ready.Preview.Kind != "issue" {
		t.Fatalf("ready=%+v", ready)
	}
	reloaded.rmu.Lock()
	inbound := reloaded.requests.Requests["request-inbound"].Inbound
	reloaded.rmu.Unlock()
	if !inbound {
		t.Fatal("legacy inbound receipt did not mark the request as inbound")
	}
}
