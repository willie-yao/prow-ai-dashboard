package remediation

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

const (
	StatusOpen           = "open"
	StatusClosedUnmerged = "closed_unmerged"
	StatusMerged         = "merged"
	StatusInconclusive   = "inconclusive"
)

// FixReference is a dashboard-created fix recorded by the fix-PR manager.
type FixReference struct {
	URL       string
	OpenedAt  string
	PatchHash string
}

// PullRequestClient is the GitHub lifecycle subset used by reconciliation.
type PullRequestClient interface {
	GetPullRequest(ctx context.Context, owner, repo string, number int) (ghpr.PullRequest, error)
}

// PullRequestSearchClient recovers dashboard-created pull requests by marker.
type PullRequestSearchClient interface {
	SearchPR(ctx context.Context, owner, repo, queryToken, confirmMarker string) (int, string, bool, error)
}

// Reconciler updates the remediation ledger from GitHub state.
type Reconciler struct {
	client      PullRequestClient
	dataDir     string
	backend     storage.Backend
	catalog     *jobconfig.Catalog
	coverage    *CoverageCatalog
	compare     CompareClient
	issues      map[string]IssueRef
	issueClient IssueLifecycleClient
	issueRepo   string
	targetRepo  string
	search      PullRequestSearchClient
}

// NewReconciler builds a lifecycle reconciler.
func NewReconciler(client PullRequestClient, dataDir string) *Reconciler {
	return &Reconciler{client: client, dataDir: dataDir}
}

// SetVerification enables Prow observations after GitHub lifecycle updates.
func (r *Reconciler) SetVerification(backend storage.Backend, catalog *jobconfig.Catalog, coverage *CoverageCatalog, compare CompareClient) {
	r.backend, r.catalog, r.coverage, r.compare = backend, catalog, coverage, compare
}

// SetIssues links pattern job IDs to tracked issues and enables lifecycle updates.
func (r *Reconciler) SetIssues(repo string, issues map[string]IssueRef, client IssueLifecycleClient) {
	r.issueRepo, r.issues, r.issueClient = repo, issues, client
}

// SetRecovery enables marker-based pull request adoption.
func (r *Reconciler) SetRecovery(targetRepo string, search PullRequestSearchClient) {
	r.targetRepo, r.search = targetRepo, search
}

// Reconcile attaches tracked fixes to findings and refreshes every known pull request.
func (r *Reconciler) Reconcile(ctx context.Context, patterns []models.PatternAnalysis, details []models.JobDetail, fixes map[string]FixReference, keyFor func(models.PatternAnalysis) string) (*State, error) {
	state := Load(r.dataDir)
	var errs []error

	for _, pattern := range patterns {
		currentID := pattern.ID
		if currentID == "" {
			currentID = models.PatternID(pattern)
		}
		currentEvidence := EvidenceForPattern(pattern, details)
		entry := state.Remediations[currentID]
		if entry == nil {
			for _, existing := range state.Remediations {
				if existing != nil && existing.JobID == pattern.JobID && evidenceOverlaps(existing.Evidence, currentEvidence) {
					entry = existing
					entry.FindingID = currentID
					entry.UpdatedAt = nowString()
					break
				}
			}
		}
		key := keyFor(pattern)
		fix, ok := fixes[key]
		if (!ok || strings.TrimSpace(fix.URL) == "") && r.search != nil && r.targetRepo != "" {
			owner, repo, valid := strings.Cut(r.targetRepo, "/")
			if valid {
				_, pullURL, found, err := r.search.SearchPR(ctx, owner, repo, fixpr.MarkerToken(key), fixpr.MarkerFor(key))
				if err != nil {
					errs = append(errs, fmt.Errorf("recover remediation pull request: %w", err))
				} else if found {
					fix, ok = FixReference{URL: pullURL, OpenedAt: nowString()}, true
				}
			}
		}
		if !ok || strings.TrimSpace(fix.URL) == "" {
			continue
		}
		id := currentID
		if entry == nil {
			now := nowString()
			entry = &Remediation{
				ID: id, FindingID: id, Subject: pattern.Subject, JobID: pattern.JobID,
				JobName: pattern.Subject, Classification: classificationForPattern(pattern, details),
				Evidence: currentEvidence, CreatedAt: now, UpdatedAt: now,
			}
			for _, detail := range details {
				if detail.JobID == pattern.JobID || detail.Name == pattern.Subject {
					entry.JobName, entry.JobType = detail.Name, detail.JobType
					entry.SourceRepo = sourceRepoFromDetail(detail, "")
					break
				}
			}
			state.Remediations[id] = entry
		}
		if issue, ok := r.issues[pattern.JobID]; ok {
			copy := issue
			entry.Issue = &copy
		}
		if findAttempt(entry, fix.URL) == nil {
			owner, repo, number, err := ParsePullRequestURL(fix.URL)
			if err != nil {
				errs = append(errs, fmt.Errorf("remediation %s: %w", id, err))
				continue
			}
			parent := 0
			if len(entry.Attempts) > 0 {
				parent = entry.Attempts[len(entry.Attempts)-1].Number
			}
			entry.Attempts = append(entry.Attempts, Attempt{
				Number: len(entry.Attempts) + 1, ParentAttempt: parent, PRNumber: number, URL: fix.URL,
				TargetRepo: owner + "/" + repo, OpenedAt: fix.OpenedAt, PatchHash: fix.PatchHash, Status: StatusOpen,
			})
		}
	}

	ids := make([]string, 0, len(state.Remediations))
	for id := range state.Remediations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		entry := state.Remediations[id]
		if entry == nil || len(entry.Attempts) == 0 {
			continue
		}
		attempt := &entry.Attempts[len(entry.Attempts)-1]
		owner, repo, number, err := ParsePullRequestURL(attempt.URL)
		if err != nil {
			errs = append(errs, fmt.Errorf("remediation %s: %w", id, err))
			continue
		}
		pull, err := r.client.GetPullRequest(ctx, owner, repo, number)
		if err != nil {
			errs = append(errs, fmt.Errorf("remediation %s pull request: %w", id, err))
			continue
		}
		applyPullRequest(entry, attempt, pull)
		if entry.SourceRepo == "" {
			entry.SourceRepo = testedRepoFor(entry, attempt.TargetRepo, r.catalog)
		}
		if entry.SourceRepo == "" {
			if detail := findJobDetail(entry, details); detail != nil {
				entry.SourceRepo = sourceRepoFromDetail(*detail, attempt.TargetRepo)
			}
		}
		if r.backend != nil && attempt.PRState == StatusOpen {
			if err := ObservePresubmits(ctx, r.backend, entry, attempt, r.coverage); err != nil {
				errs = append(errs, fmt.Errorf("remediation %s presubmit: %w", id, err))
			}
		}
		if r.compare != nil && attempt.PRState == StatusMerged {
			minCleanRuns := 2
			if entry.Classification == string(models.ClassificationFlaky) {
				minCleanRuns = 10
			}
			if err := ObservePeriodic(ctx, r.compare, entry, attempt, details, minCleanRuns); err != nil {
				errs = append(errs, fmt.Errorf("remediation %s periodic: %w", id, err))
			}
		}
		issueClient := r.issueClient
		if entry.Issue != nil && entry.Issue.Repo != "" && entry.Issue.Repo != r.issueRepo {
			issueClient = nil
		}
		if err := reconcileIssue(ctx, issueClient, entry, attempt); err != nil {
			errs = append(errs, fmt.Errorf("remediation %s issue: %w", id, err))
		}
	}
	if err := state.Save(r.dataDir); err != nil {
		errs = append(errs, err)
	}
	return state, errors.Join(errs...)
}

func evidenceOverlaps(left, right Evidence) bool {
	for _, a := range left.Tests {
		for _, b := range right.Tests {
			if a.Identity == b.Identity && a.ErrorHash != "" && a.ErrorHash == b.ErrorHash {
				return true
			}
		}
	}
	return false
}

// ParsePullRequestURL extracts owner, repository, and number from a GitHub URL.
func ParsePullRequestURL(value string) (string, string, int, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", "", 0, fmt.Errorf("parse pull request URL: %w", err)
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return "", "", 0, fmt.Errorf("pull request URL %q has unexpected path", value)
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number <= 0 {
		return "", "", 0, fmt.Errorf("pull request URL %q has invalid number", value)
	}
	return parts[0], parts[1], number, nil
}

func findAttempt(remediation *Remediation, pullURL string) *Attempt {
	for i := range remediation.Attempts {
		if remediation.Attempts[i].URL == pullURL {
			return &remediation.Attempts[i]
		}
	}
	return nil
}

func applyPullRequest(remediation *Remediation, attempt *Attempt, pull ghpr.PullRequest) {
	previousStatus := attempt.Status
	previousHead := attempt.HeadSHA
	attempt.PRNumber = pull.Number
	attempt.URL = pull.HTMLURL
	attempt.TargetRepo = pull.Base.Repo
	attempt.HeadRepo, attempt.HeadRef, attempt.HeadSHA = pull.Head.Repo, pull.Head.Ref, pull.Head.SHA
	attempt.BaseRef, attempt.BaseSHA = pull.Base.Ref, pull.Base.SHA
	attempt.MergeSHA = pull.MergeCommitSHA
	if !pull.MergedAt.IsZero() {
		attempt.MergedAt = pull.MergedAt.UTC().Format(timeFormat)
	}
	switch {
	case pull.Merged:
		attempt.PRState = StatusMerged
	case pull.State == "closed":
		attempt.PRState = StatusClosedUnmerged
	default:
		attempt.PRState = StatusOpen
	}
	if previousHead != "" && previousHead != attempt.HeadSHA && attempt.PRState == StatusOpen {
		attempt.Observations = nil
		attempt.Outcome, attempt.OutcomeReason = "", ""
		attempt.Status = StatusOpen
	} else {
		switch attempt.PRState {
		case StatusMerged:
			if !postMergeStatus(attempt.Status) {
				attempt.Status = StatusMerged
			}
		case StatusClosedUnmerged:
			attempt.Status = StatusClosedUnmerged
		case StatusOpen:
			if !preMergeStatus(attempt.Status) {
				attempt.Status = StatusOpen
			}
		}
	}
	if previousStatus != attempt.Status {
		attempt.LastTransition = previousStatus + "->" + attempt.Status
		attempt.TransitionIndex++
		remediation.LastTransition = attempt.LastTransition
	}
	remediation.UpdatedAt = nowString()
}

func preMergeStatus(status string) bool {
	switch status {
	case StatusAwaitingPresubmit, StatusPresubmitRunning, StatusPremergeVerified,
		StatusPresubmitFailedSameCause, StatusPresubmitFailedDifferentCause, StatusInconclusive:
		return true
	default:
		return false
	}
}

func postMergeStatus(status string) bool {
	switch status {
	case StatusObserving, StatusVerifiedFixed, StatusStillFailingSameCause,
		StatusFailingDifferentCause, StatusInconclusive:
		return true
	default:
		return false
	}
}

func sourceRepoFromDetail(detail models.JobDetail, targetRepo string) string {
	if detail.Repo != "" {
		return detail.Repo
	}
	repos := map[string]struct{}{}
	for _, run := range detail.Runs {
		for repo := range run.RepoRefs {
			if repo == targetRepo && targetRepo != "" {
				return repo
			}
			if strings.Contains(repo, "/") {
				repos[repo] = struct{}{}
			}
		}
	}
	if len(repos) == 1 {
		for repo := range repos {
			return repo
		}
	}
	return ""
}

func testedRepoFor(remediation *Remediation, targetRepo string, catalog *jobconfig.Catalog) string {
	if catalog == nil {
		return remediation.SourceRepo
	}
	definition, ok := catalog.Jobs[remediation.JobID]
	if !ok {
		for _, candidate := range catalog.Jobs {
			if candidate.Name == remediation.JobName && candidate.JobType == remediation.JobType {
				definition, ok = candidate, true
				break
			}
		}
	}
	if !ok {
		return remediation.SourceRepo
	}
	if definition.TestsRepo(targetRepo) {
		return targetRepo
	}
	if definition.JobType == models.JobTypePresubmit {
		return definition.Repo
	}
	for _, ref := range definition.Refs {
		if repo := ref.FullRepo(); repo != "" {
			return repo
		}
	}
	return remediation.SourceRepo
}

const timeFormat = "2006-01-02T15:04:05Z07:00"
