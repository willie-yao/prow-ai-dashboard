package remediation

import (
	"context"
	"fmt"
)

// IssueLifecycleClient updates a linked issue after verification transitions.
type IssueLifecycleClient interface {
	CommentIssue(ctx context.Context, number int, body string) error
	CloseIssue(ctx context.Context, number int) error
}

func reconcileIssue(ctx context.Context, client IssueLifecycleClient, remediation *Remediation, attempt *Attempt) error {
	if client == nil || remediation == nil || remediation.Issue == nil || attempt == nil {
		return nil
	}
	issue := remediation.Issue
	if issue.State == "closed" || attempt.LastTransition == "" {
		return nil
	}
	alreadyCommented := issue.LastTransition == attempt.LastTransition
	if alreadyCommented && attempt.Status != StatusVerifiedFixed {
		return nil
	}
	var body string
	switch attempt.Status {
	case StatusPremergeVerified:
		body = fmt.Sprintf("The proposed fix passed its Prow presubmit verification on the current pull request head: %s", attempt.URL)
	case StatusMerged, StatusObserving:
		body = fmt.Sprintf("The proposed fix merged. The dashboard is now observing subsequent Prow runs: %s", attempt.URL)
	case StatusPresubmitFailedSameCause:
		body = fmt.Sprintf("The proposed fix reproduced the same failure in Prow presubmit verification: %s", attempt.URL)
	case StatusStillFailingSameCause:
		body = fmt.Sprintf("The same failure signature recurred after the fix merged. The prior fix did not resolve the finding: %s", attempt.URL)
	case StatusVerifiedFixed:
		body = fmt.Sprintf("The dashboard verified the fix across post-merge Prow runs: %s", attempt.URL)
	default:
		issue.LastTransition = attempt.LastTransition
		return nil
	}
	if !alreadyCommented {
		if err := client.CommentIssue(ctx, issue.Number, body); err != nil {
			return err
		}
		issue.LastTransition = attempt.LastTransition
	}
	if attempt.Status == StatusVerifiedFixed {
		if err := client.CloseIssue(ctx, issue.Number); err != nil {
			return err
		}
		issue.State = "closed"
	}
	return nil
}
