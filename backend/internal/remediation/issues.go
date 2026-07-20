package remediation

import (
	"context"
	"errors"
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

func reconcileLinkedIssues(ctx context.Context, client IssueLifecycleClient, repo string, state *State) error {
	if client == nil || state == nil {
		return nil
	}
	groups := map[string][]*Remediation{}
	for _, entry := range state.Remediations {
		if entry == nil || entry.Issue == nil || entry.Issue.Number == 0 || entry.Issue.Repo != repo || len(entry.Attempts) == 0 {
			continue
		}
		key := fmt.Sprintf("%s#%d", entry.Issue.Repo, entry.Issue.Number)
		groups[key] = append(groups[key], entry)
	}
	var errs []error
	for _, entries := range groups {
		pending := false
		var closureOwner *Remediation
		for _, entry := range entries {
			attempt := &entry.Attempts[len(entry.Attempts)-1]
			if attempt.Status == StatusVerifiedFixed {
				if closureOwner == nil || entry.UpdatedAt > closureOwner.UpdatedAt {
					closureOwner = entry
				}
				continue
			}
			if attempt.Status != StatusClosedUnmerged {
				pending = true
			}
			if err := reconcileIssue(ctx, client, entry, attempt); err != nil {
				errs = append(errs, err)
			}
		}
		if pending || closureOwner == nil {
			continue
		}
		attempt := &closureOwner.Attempts[len(closureOwner.Attempts)-1]
		if err := reconcileIssue(ctx, client, closureOwner, attempt); err != nil {
			errs = append(errs, err)
			continue
		}
		if closureOwner.Issue.State == "closed" {
			for _, entry := range entries {
				entry.Issue.State = "closed"
				entry.Issue.LastTransition = closureOwner.Issue.LastTransition
			}
		}
	}
	return errors.Join(errs...)
}
