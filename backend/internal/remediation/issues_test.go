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
