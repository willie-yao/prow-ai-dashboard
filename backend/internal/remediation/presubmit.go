package remediation

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aggregator"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/junit"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

const (
	StatusAwaitingPresubmit             = "awaiting_presubmit"
	StatusPresubmitRunning              = "presubmit_running"
	StatusPremergeVerified              = "premerge_verified"
	StatusPresubmitFailedSameCause      = "presubmit_failed_same_cause"
	StatusPresubmitFailedDifferentCause = "presubmit_failed_different_cause"

	OutcomePassed         = "passed"
	OutcomeSameCause      = "same_cause"
	OutcomeDifferentCause = "different_cause"
	OutcomeInconclusive   = "inconclusive"
	OutcomePending        = "pending"
)

// ObservePresubmits evaluates verification jobs for the current pull head.
func ObservePresubmits(ctx context.Context, b storage.Backend, remediation *Remediation, attempt *Attempt, coverage *CoverageCatalog) error {
	if remediation == nil || attempt == nil || attempt.PRNumber <= 0 || attempt.HeadSHA == "" {
		return nil
	}
	jobs := verificationJobs(remediation, coverage)
	if len(jobs) == 0 {
		return nil
	}
	matching := jobs[:0]
	for _, job := range jobs {
		if job.Repo == attempt.TargetRepo {
			matching = append(matching, job)
		}
	}
	if len(matching) == 0 {
		transitionAttempt(remediation, attempt, StatusInconclusive, OutcomeTargetRepoNotTested,
			fmt.Sprintf("pull request targets %s but verification jobs test another repository", attempt.TargetRepo))
		return nil
	}
	jobs = matching
	var observations []BuildObservation
	for _, job := range jobs {
		jobEvidence := evidenceForVerificationJob(remediation.Evidence, coverage, job)
		builds, err := prowbuild.ListPullBuilds(ctx, b, job.Repo, strconv.Itoa(attempt.PRNumber), job.JobName, 5)
		if err != nil {
			return fmt.Errorf("list pull builds for %s: %w", job.JobName, err)
		}
		for _, build := range builds {
			loc := prowbuild.BuildLocation{
				JobLocation: prowbuild.JobLocation{JobType: models.JobTypePresubmit, Repo: job.Repo},
				JobName:     job.JobName, BuildID: build.ID, PullNumber: build.PullNumber,
			}
			metadata, metadataErr := prowbuild.FetchProwJobMetadata(ctx, b, loc)
			if metadataErr == nil && metadata.Refs != nil && len(metadata.Refs.Pulls) > 0 {
				headSHA := metadata.Refs.Pulls[0].SHA
				if headSHA != attempt.HeadSHA {
					continue
				}
				observation := BuildObservation{
					BuildID: build.ID, JobName: job.JobName, JobType: models.JobTypePresubmit,
					PullNumber: attempt.PRNumber, SourceRepo: job.Repo, HeadSHA: headSHA,
					Result: strings.ToUpper(metadata.State), ProwURL: metadata.URL,
				}
				if !metadata.StartTime.IsZero() {
					observation.StartedAt = metadata.StartTime.UTC().Format(timeFormat)
				}
				if !metadata.Completion.IsZero() {
					observation.CompletedAt = metadata.Completion.UTC().Format(timeFormat)
				}
				if metadata.State == "pending" || metadata.State == "triggered" {
					observation.Outcome = OutcomePending
					observations = append(observations, observation)
					break
				}
				info, cases, err := fetchBuildTests(ctx, b, loc)
				if err != nil {
					observation.Outcome, observation.Reason = OutcomeInconclusive, err.Error()
					observations = append(observations, observation)
					break
				}
				observation.Result = info.Result
				classifyObservation(jobEvidence, cases, info.Passed, &observation)
				observations = append(observations, observation)
				break
			}

			info, cases, err := fetchBuildTests(ctx, b, loc)
			if err != nil || info.Revision == "" || info.Revision != attempt.HeadSHA {
				continue
			}
			observation := BuildObservation{
				BuildID: build.ID, JobName: job.JobName, JobType: models.JobTypePresubmit,
				PullNumber: attempt.PRNumber, SourceRepo: job.Repo, HeadSHA: info.Revision,
				Result: info.Result, ProwURL: info.ProwURL,
			}
			classifyObservation(jobEvidence, cases, info.Passed, &observation)
			observations = append(observations, observation)
			break
		}
	}
	mergeObservations(attempt, observations)
	applyPresubmitOutcome(remediation, attempt, jobs, currentPresubmitObservations(attempt, jobs))
	return nil
}

func verificationJobs(remediation *Remediation, coverage *CoverageCatalog) []VerificationJob {
	byID := map[string]VerificationJob{}
	if remediation.JobType == models.JobTypePresubmit && remediation.SourceRepo != "" && remediation.JobName != "" {
		job := VerificationJob{
			JobID:   models.JobIDFor(models.JobTypePresubmit, remediation.SourceRepo, remediation.JobName),
			JobName: remediation.JobName, Repo: remediation.SourceRepo,
			RerunCommand: "/test " + remediation.JobName,
		}
		byID[job.JobID] = job
	}
	if coverage != nil {
		for _, evidence := range remediation.Evidence.Tests {
			for _, job := range coverage.Tests[evidence.Identity] {
				byID[job.JobID] = job
			}
		}
	}
	out := make([]VerificationJob, 0, len(byID))
	for _, job := range byID {
		out = append(out, job)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JobID < out[j].JobID })
	return out
}

func evidenceForVerificationJob(evidence Evidence, coverage *CoverageCatalog, job VerificationJob) Evidence {
	if coverage == nil {
		return evidence
	}
	out := evidence
	out.Tests = nil
	for _, test := range evidence.Tests {
		for _, candidate := range coverage.Tests[test.Identity] {
			if candidate.JobID == job.JobID {
				out.Tests = append(out.Tests, test)
				break
			}
		}
	}
	if len(out.Tests) == 0 {
		return evidence
	}
	return out
}

func fetchBuildTests(ctx context.Context, b storage.Backend, loc prowbuild.BuildLocation) (*models.BuildInfo, []models.TestCase, error) {
	info, err := prowbuild.FetchBuildInfo(ctx, b, loc)
	if err != nil {
		return nil, nil, err
	}
	paths, err := prowbuild.DiscoverJUnitPaths(ctx, b, loc)
	if err != nil {
		return info, nil, err
	}
	var cases []models.TestCase
	for _, path := range paths {
		data, err := storage.ReadAll(ctx, b, path)
		if err != nil {
			return info, nil, err
		}
		parsed, err := junit.ParseFile(data, filepath.Base(path))
		if err != nil {
			return info, nil, err
		}
		cases = append(cases, parsed...)
	}
	if len(cases) == 0 {
		return info, nil, fmt.Errorf("no JUnit tests found")
	}
	return info, cases, nil
}

func classifyObservation(evidence Evidence, cases []models.TestCase, jobPassed bool, observation *BuildObservation) {
	if len(evidence.Tests) == 0 {
		if jobPassed {
			observation.Outcome = OutcomePassed
		} else {
			observation.Outcome, observation.Reason = OutcomeInconclusive, "job failed without test-level evidence"
		}
		return
	}
	byIdentity := make(map[string][]models.TestCase, len(cases))
	for _, test := range cases {
		for _, identity := range junit.Identities(test) {
			byIdentity[identity] = append(byIdentity[identity], test)
		}
	}
	missing := 0
	differentFailure := false
	for _, expected := range evidence.Tests {
		matched := byIdentity[expected.Identity]
		if len(matched) == 0 {
			missing++
			continue
		}
		executed := false
		for _, test := range matched {
			if test.Status == "skipped" {
				continue
			}
			executed = true
			if test.Status != "failed" {
				continue
			}
			hash := aggregator.HashError(aggregator.NormalizeErrorMessage(test.FailureMessage + "\n" + test.FailureBody))
			if expected.ErrorHash != "" && hash == expected.ErrorHash {
				observation.FailedMatches = append(observation.FailedMatches, expected.Identity)
				observation.Outcome, observation.Reason = OutcomeSameCause, "original failure signature recurred"
				return
			}
			differentFailure = true
		}
		if !executed {
			missing++
			continue
		}
		observation.MatchedTests = append(observation.MatchedTests, expected.Identity)
	}
	sort.Strings(observation.MatchedTests)
	if missing > 0 {
		observation.Outcome = OutcomeInconclusive
		observation.Reason = fmt.Sprintf("%d expected test(s) did not execute", missing)
		return
	}
	if differentFailure {
		observation.Outcome, observation.Reason = OutcomeDifferentCause, "expected test failed with a different signature"
		return
	}
	observation.Outcome = OutcomePassed
}

func mergeObservations(attempt *Attempt, observations []BuildObservation) {
	for _, observation := range observations {
		found := false
		for i := range attempt.Observations {
			if attempt.Observations[i].BuildID == observation.BuildID && attempt.Observations[i].JobName == observation.JobName {
				attempt.Observations[i] = observation
				found = true
				break
			}
		}
		if !found {
			attempt.Observations = append(attempt.Observations, observation)
		}
	}
}

func currentPresubmitObservations(attempt *Attempt, jobs []VerificationJob) []BuildObservation {
	selected := map[string]bool{}
	for _, job := range jobs {
		selected[job.JobName] = true
	}
	var out []BuildObservation
	for _, observation := range attempt.Observations {
		if observation.JobType == models.JobTypePresubmit && observation.HeadSHA == attempt.HeadSHA && selected[observation.JobName] {
			out = append(out, observation)
		}
	}
	return out
}

func applyPresubmitOutcome(remediation *Remediation, attempt *Attempt, jobs []VerificationJob, observations []BuildObservation) {
	previous := attempt.Status
	if len(observations) == 0 {
		attempt.Status = StatusAwaitingPresubmit
		attempt.OutcomeReason = "waiting for Prow presubmit"
		for _, job := range jobs {
			if job.RerunCommand != "" {
				attempt.OutcomeReason = "waiting for " + job.RerunCommand
				break
			}
		}
	} else {
		observedJobs := map[string]bool{}
		for _, observation := range observations {
			observedJobs[observation.JobName] = true
		}
		allPassed := len(observedJobs) == len(jobs)
		for _, observation := range observations {
			switch observation.Outcome {
			case OutcomeSameCause:
				attempt.Outcome, attempt.OutcomeReason = OutcomeSameCause, observation.Reason
			case OutcomeDifferentCause:
				if attempt.Outcome != OutcomeSameCause {
					attempt.Outcome, attempt.OutcomeReason = OutcomeDifferentCause, observation.Reason
				}
			case OutcomePending:
				attempt.Status = StatusPresubmitRunning
				allPassed = false
			case OutcomePassed:
			default:
				allPassed = false
				if attempt.Outcome == "" {
					attempt.Outcome, attempt.OutcomeReason = OutcomeInconclusive, observation.Reason
				}
			}
			if observation.Outcome != OutcomePassed {
				allPassed = false
			}
		}
		if attempt.Outcome == OutcomeSameCause {
			attempt.Status = StatusPresubmitFailedSameCause
		} else if attempt.Outcome == OutcomeDifferentCause {
			attempt.Status = StatusPresubmitFailedDifferentCause
		} else if allPassed {
			attempt.Status, attempt.Outcome = StatusPremergeVerified, OutcomePassed
		} else if attempt.Status != StatusPresubmitRunning {
			attempt.Status = StatusAwaitingPresubmit
			attempt.OutcomeReason = "waiting for all matching Prow presubmits"
		}
	}
	if previous != attempt.Status {
		attempt.LastTransition = previous + "->" + attempt.Status
		attempt.TransitionIndex++
		remediation.LastTransition = attempt.LastTransition
	}
	attempt.LastObservedAt = nowString()
	remediation.UpdatedAt = nowString()
}
