package remediation

import (
	"context"
	"testing"
)

type fakeIssueLifecycle struct {
	comments []string
	closed   []int
}

func (f *fakeIssueLifecycle) CommentIssue(_ context.Context, _ int, body string) error {
	f.comments = append(f.comments, body)
	return nil
}
func (f *fakeIssueLifecycle) CloseIssue(_ context.Context, number int) error {
	f.closed = append(f.closed, number)
	return nil
}

func TestReconcileIssueClosesVerifiedFix(t *testing.T) {
	client := &fakeIssueLifecycle{}
	remediation := &Remediation{Issue: &IssueRef{Number: 9}}
	attempt := &Attempt{Status: StatusVerifiedFixed, URL: "https://github.com/o/r/pull/7", LastTransition: "observing->verified_fixed"}
	if err := reconcileIssue(context.Background(), client, remediation, attempt); err != nil {
		t.Fatal(err)
	}
	if len(client.comments) != 1 || len(client.closed) != 1 || remediation.Issue.State != "closed" {
		t.Fatalf("comments=%v closed=%v issue=%+v", client.comments, client.closed, remediation.Issue)
	}
	if err := reconcileIssue(context.Background(), client, remediation, attempt); err != nil {
		t.Fatal(err)
	}
	if len(client.comments) != 1 {
		t.Fatal("duplicate transition comment")
	}
}

func TestReconcileLinkedIssuesWaitsForEveryFinding(t *testing.T) {
	client := &fakeIssueLifecycle{}
	state := NewState()
	state.Remediations["old"] = &Remediation{
		UpdatedAt: "2026-07-20T01:00:00Z", Issue: &IssueRef{Number: 9, Repo: "o/r"},
		Attempts: []Attempt{{Status: StatusVerifiedFixed, LastTransition: "observing->verified_fixed"}},
	}
	state.Remediations["new"] = &Remediation{
		UpdatedAt: "2026-07-20T02:00:00Z", Issue: &IssueRef{Number: 9, Repo: "o/r"},
		Attempts: []Attempt{{Status: StatusObserving, LastTransition: "merged->observing"}},
	}
	if err := reconcileLinkedIssues(context.Background(), client, "o/r", state); err != nil {
		t.Fatal(err)
	}
	if len(client.closed) != 0 {
		t.Fatalf("issue closed while a finding was pending: %v", client.closed)
	}
	state.Remediations["new"].Attempts[0].Status = StatusVerifiedFixed
	state.Remediations["new"].Attempts[0].LastTransition = "observing->verified_fixed"
	if err := reconcileLinkedIssues(context.Background(), client, "o/r", state); err != nil {
		t.Fatal(err)
	}
	if len(client.closed) != 1 {
		t.Fatalf("issue close calls = %v", client.closed)
	}
}
