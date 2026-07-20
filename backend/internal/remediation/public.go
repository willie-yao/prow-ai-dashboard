package remediation

// PublicFileName is the redacted remediation status consumed by the frontend.
const PublicFileName = "remediations.json"

// PublicState is the redacted remediation projection.
type PublicState struct {
	Remediations map[string]PublicRemediation `json:"remediations"`
}

// PublicRemediation exposes links and status without private generation data.
type PublicRemediation struct {
	ID        string         `json:"id"`
	Subject   string         `json:"subject"`
	JobID     string         `json:"job_id"`
	JobName   string         `json:"job_name"`
	JobType   string         `json:"job_type"`
	Issue     *IssueRef      `json:"issue,omitempty"`
	Attempt   *PublicAttempt `json:"attempt,omitempty"`
	UpdatedAt string         `json:"updated_at"`
}

// PublicAttempt is the latest pull request verification state.
type PublicAttempt struct {
	Number        int                `json:"number"`
	PRNumber      int                `json:"pr_number"`
	URL           string             `json:"url"`
	TargetRepo    string             `json:"target_repo"`
	HeadSHA       string             `json:"head_sha,omitempty"`
	MergeSHA      string             `json:"merge_sha,omitempty"`
	Status        string             `json:"status"`
	PRState       string             `json:"pr_state,omitempty"`
	Outcome       string             `json:"outcome,omitempty"`
	OutcomeReason string             `json:"outcome_reason,omitempty"`
	Observations  []BuildObservation `json:"observations,omitempty"`
}

// Public returns the redacted frontend projection.
func (s *State) Public() PublicState {
	out := PublicState{Remediations: map[string]PublicRemediation{}}
	if s == nil {
		return out
	}
	for id, entry := range s.Remediations {
		if entry == nil {
			continue
		}
		public := PublicRemediation{
			ID: entry.ID, Subject: entry.Subject, JobID: entry.JobID,
			JobName: entry.JobName, JobType: entry.JobType, UpdatedAt: entry.UpdatedAt,
		}
		if entry.Issue != nil {
			issue := *entry.Issue
			issue.Repo = ""
			issue.LastTransition = ""
			public.Issue = &issue
		}
		if len(entry.Attempts) > 0 {
			attempt := entry.Attempts[len(entry.Attempts)-1]
			public.Attempt = &PublicAttempt{
				Number: attempt.Number, PRNumber: attempt.PRNumber, URL: attempt.URL,
				TargetRepo: attempt.TargetRepo, HeadSHA: attempt.HeadSHA, MergeSHA: attempt.MergeSHA,
				Status: attempt.Status, PRState: attempt.PRState,
				Outcome: attempt.Outcome, OutcomeReason: attempt.OutcomeReason,
				Observations: append([]BuildObservation(nil), attempt.Observations...),
			}
		}
		out.Remediations[id] = public
	}
	return out
}
