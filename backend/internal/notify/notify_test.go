package notify

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

type fakeSender struct {
	messages []Message
	failNext int
}

func (f *fakeSender) Send(_ context.Context, message Message) error {
	if f.failNext > 0 {
		f.failNext--
		return errors.New("delivery failed")
	}
	f.messages = append(f.messages, message)
	return nil
}

func newTestNotifier(t *testing.T, sender Sender, stateFile string) *Notifier {
	t.Helper()
	from, to, err := ParseAddresses("Prow Dashboard <prow@example.com>", []string{"team@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	return NewNotifier(sender, from, to, stateFile, "Example", "https://dash.example.com", "https://prow.example.com/view/")
}

func makeReport(failures ...models.TestFlakiness) models.FlakinessReport {
	return models.FlakinessReport{PersistentFailures: failures}
}

func persistentFailure(jobID, jobName, testName, hash string, count int) models.TestFlakiness {
	return models.TestFlakiness{
		JobID:               jobID,
		JobName:             jobName,
		TestName:            testName,
		ConsecutiveFailures: count,
		LastFailure: &models.TestFailureInfo{
			BuildID:        "100",
			FailureMessage: "context deadline exceeded",
			ErrorHash:      hash,
		},
	}
}

func TestStateSaveLoad(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	n := newTestNotifier(t, &fakeSender{}, stateFile)
	n.state.Notified["job1::test1"] = NotifiedFailure{ErrorHash: "abc", JobName: "job1", TestName: "test1"}
	if err := n.SaveState(); err != nil {
		t.Fatal(err)
	}

	n2 := newTestNotifier(t, &fakeSender{}, stateFile)
	if n2.state.Channel != notificationChannel || len(n2.state.Notified) != 1 {
		t.Fatalf("state = %+v", n2.state)
	}
}

func TestLegacyStateResetsForEmailChannel(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	legacy := NotificationState{Notified: map[string]NotifiedFailure{"job::test": {ErrorHash: "old"}}}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(stateFile, data, 0o644); err != nil {
		t.Fatal(err)
	}

	n := newTestNotifier(t, &fakeSender{}, stateFile)
	if n.state.Channel != notificationChannel || len(n.state.Notified) != 0 {
		t.Fatalf("legacy state was not reset: %+v", n.state)
	}
}

func TestNewPersistentFailure(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	failure := persistentFailure("job-id", "job", "TestSomething", "hash1", 5)

	stats, err := n.ProcessFailures(context.Background(), makeReport(failure), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.NewAlerts != 1 || stats.Failed != 0 || len(sender.messages) != 1 {
		t.Fatalf("stats=%+v messages=%d", stats, len(sender.messages))
	}
	if got := n.state.Notified["job-id::TestSomething"]; got.ErrorHash != "hash1" {
		t.Fatalf("state = %+v", got)
	}
}

func TestSameFailureIsNotRepeated(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	failure := persistentFailure("job-id", "job", "TestSomething", "hash1", 3)
	if _, err := n.ProcessFailures(context.Background(), makeReport(failure), nil); err != nil {
		t.Fatal(err)
	}
	failure.ConsecutiveFailures = 7
	stats, err := n.ProcessFailures(context.Background(), makeReport(failure), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.NewAlerts != 0 || len(sender.messages) != 1 {
		t.Fatalf("stats=%+v messages=%d", stats, len(sender.messages))
	}
	if got := n.state.Notified["job-id::TestSomething"].ConsecutiveCount; got != 7 {
		t.Fatalf("consecutive count = %d", got)
	}
}

func TestChangedErrorSendsAgain(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	failure := persistentFailure("job-id", "job", "TestSomething", "hash1", 3)
	if _, err := n.ProcessFailures(context.Background(), makeReport(failure), nil); err != nil {
		t.Fatal(err)
	}
	failure.LastFailure.ErrorHash = "hash2"
	stats, err := n.ProcessFailures(context.Background(), makeReport(failure), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.NewAlerts != 1 || len(sender.messages) != 2 {
		t.Fatalf("stats=%+v messages=%d", stats, len(sender.messages))
	}
	if !strings.Contains(sender.messages[1].Subject, "Failure changed") {
		t.Fatalf("subject = %q", sender.messages[1].Subject)
	}
}

func TestRecoverySendsAndDeletesState(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	n.state.Notified["job-id::TestRecovered"] = NotifiedFailure{
		ConsecutiveCount: 7,
		ErrorHash:        "hash",
		JobName:          "job",
		TestName:         "TestRecovered",
	}

	stats, err := n.ProcessFailures(context.Background(), makeReport(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Recoveries != 1 || len(sender.messages) != 1 {
		t.Fatalf("stats=%+v messages=%d", stats, len(sender.messages))
	}
	if len(n.state.Notified) != 0 {
		t.Fatalf("state not cleared: %+v", n.state.Notified)
	}
}

func TestBelowThresholdIgnored(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	failure := persistentFailure("job-id", "job", "TestSomething", "hash", 2)

	stats, err := n.ProcessFailures(context.Background(), makeReport(failure), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (Stats{}) || len(sender.messages) != 0 {
		t.Fatalf("stats=%+v messages=%d", stats, len(sender.messages))
	}
}

func TestJobIDSeparatesSameNames(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	one := persistentFailure("periodic/job", "same-job", "TestSame", "hash", 3)
	two := persistentFailure("presubmit/repo/1/job", "same-job", "TestSame", "hash", 3)

	stats, err := n.ProcessFailures(context.Background(), makeReport(one, two), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.NewAlerts != 2 || len(n.state.Notified) != 2 {
		t.Fatalf("stats=%+v state=%+v", stats, n.state.Notified)
	}
}

func TestAILookupUsesNewestFailedRun(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	failure := persistentFailure("job-id", "job", "TestAI", "hash", 4)
	details := []models.JobDetail{{
		JobID: "job-id",
		Runs: []models.BuildResult{
			{TestCases: []models.TestCase{{Name: "TestAI", Status: "failed", AIAnalysis: &models.AIAnalysis{RootCause: "new root cause"}}}},
			{TestCases: []models.TestCase{{Name: "TestAI", Status: "failed", AIAnalysis: &models.AIAnalysis{RootCause: "old root cause"}}}},
		},
	}}

	if _, err := n.ProcessFailures(context.Background(), makeReport(failure), details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sender.messages[0].TextBody, "new root cause") || strings.Contains(sender.messages[0].TextBody, "old root cause") {
		t.Fatalf("body = %s", sender.messages[0].TextBody)
	}
}

func TestFailedNewAlertRetries(t *testing.T) {
	sender := &fakeSender{failNext: 1}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	failure := persistentFailure("job-id", "job", "TestRetry", "hash", 3)

	stats, err := n.ProcessFailures(context.Background(), makeReport(failure), nil)
	if err == nil || stats.Failed != 1 || len(n.state.Notified) != 0 {
		t.Fatalf("first pass stats=%+v err=%v state=%+v", stats, err, n.state.Notified)
	}
	stats, err = n.ProcessFailures(context.Background(), makeReport(failure), nil)
	if err != nil || stats.NewAlerts != 1 || len(n.state.Notified) != 1 {
		t.Fatalf("retry stats=%+v err=%v state=%+v", stats, err, n.state.Notified)
	}
}

func TestFailedChangedAlertPreservesOldHash(t *testing.T) {
	sender := &fakeSender{}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	failure := persistentFailure("job-id", "job", "TestRetry", "old", 3)
	if _, err := n.ProcessFailures(context.Background(), makeReport(failure), nil); err != nil {
		t.Fatal(err)
	}
	sender.failNext = 1
	failure.LastFailure.ErrorHash = "new"
	if _, err := n.ProcessFailures(context.Background(), makeReport(failure), nil); err == nil {
		t.Fatal("expected delivery error")
	}
	if got := n.state.Notified["job-id::TestRetry"].ErrorHash; got != "old" {
		t.Fatalf("hash advanced after failed send: %q", got)
	}
}

func TestFailedRecoveryRetries(t *testing.T) {
	sender := &fakeSender{failNext: 1}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	n.state.Notified["job-id::TestRecovery"] = NotifiedFailure{JobName: "job", TestName: "TestRecovery", ConsecutiveCount: 4}

	if _, err := n.ProcessFailures(context.Background(), makeReport(), nil); err == nil {
		t.Fatal("expected delivery error")
	}
	if len(n.state.Notified) != 1 {
		t.Fatal("failed recovery was removed from state")
	}
	stats, err := n.ProcessFailures(context.Background(), makeReport(), nil)
	if err != nil || stats.Recoveries != 1 || len(n.state.Notified) != 0 {
		t.Fatalf("retry stats=%+v err=%v state=%+v", stats, err, n.state.Notified)
	}
}

func TestProcessContinuesAfterFailure(t *testing.T) {
	sender := &fakeSender{failNext: 1}
	n := newTestNotifier(t, sender, filepath.Join(t.TempDir(), "state.json"))
	one := persistentFailure("a", "job-a", "TestA", "hash", 3)
	two := persistentFailure("b", "job-b", "TestB", "hash", 3)

	stats, err := n.ProcessFailures(context.Background(), makeReport(one, two), nil)
	if err == nil || stats.Failed != 1 || stats.NewAlerts != 1 || len(sender.messages) != 1 {
		t.Fatalf("stats=%+v err=%v messages=%d", stats, err, len(sender.messages))
	}
}

func TestParseAddresses(t *testing.T) {
	from, recipients, err := ParseAddresses("Dashboard <dashboard@example.com>", []string{"One <one@example.com>", "two@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if from.Address != "dashboard@example.com" || len(recipients) != 2 || recipients[0].Address != "one@example.com" {
		t.Fatalf("from=%+v recipients=%+v", from, recipients)
	}
	if _, _, err := ParseAddresses("bad", []string{"one@example.com"}); err == nil {
		t.Fatal("expected invalid sender error")
	}
}

var _ Sender = (*fakeSender)(nil)
