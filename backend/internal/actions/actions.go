// Package actions performs single-failure GitHub actions on demand: filing an
// issue or drafting a fix PR for one specific pattern, using a per-user token.
// It reuses the batch issue and fix-PR engines for exactly one item, so the
// on-demand and scheduled paths stay behaviorally identical.
package actions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/issues"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediation"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/repotemplate"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/resolve"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

// ErrNotFound means no pattern in the published data matched the given id.
var ErrNotFound = errors.New("failure not found")

// ErrPreviewNotFound means the confirm token is unknown, expired, or belongs to
// a different admin.
var ErrPreviewNotFound = errors.New("preview not found or expired")

// previewTTL bounds how long a generated draft is held for confirmation.
const previewTTL = 15 * time.Minute

// AIConfig is the resolved chat-completions configuration used to draft fixes.
type AIConfig struct {
	Token    string
	Endpoint string
	Model    string
	Headers  map[string]string
}

// PreviewResult is the draft shown to the admin before they confirm it. Token
// is passed back to Confirm to file the exact previewed issue or open the exact
// previewed PR. Diff is set for fixes only.
type PreviewResult struct {
	Token string `json:"token,omitempty"`
	Kind  string `json:"kind"` // "issue" | "fix"
	Title string `json:"title"`
	Body  string `json:"body"`
	Diff  string `json:"diff,omitempty"`
	// Verify reports the pre-PR build/vet verdict for a fix ("passed" |
	// "failed" | "skipped"). Empty for an issue preview.
	VerifyStatus  string `json:"verify_status,omitempty"`
	VerifySummary string `json:"verify_summary,omitempty"`
	// VerifyOutput is the tail of the failing command's output, set only when
	// verification failed, so the reviewer sees why before confirming.
	VerifyOutput string `json:"verify_output,omitempty"`
}

// previewEntry is a cached draft awaiting confirmation. Exactly one of spec or
// fix is set, per kind. owner binds the draft to the admin who generated it.
type previewEntry struct {
	owner     string
	kind      string
	spec      issues.IssueSpec    // issue drafts
	fix       *fixpr.GeneratedFix // fix drafts
	retry     bool
	failureID string
	createdAt time.Time
}

// Service runs on-demand actions against the data written to DataDir. It reads
// jobs/*.json to resolve a failure id and reuses the issue and fix-PR state
// files alongside them. A mutex serializes state read-modify-write so
// concurrent admin requests to one server do not clobber each other; cross-
// process consistency with the fetcher/worker relies on the engines' adopt-by-
// search path, which recovers when local state is stale.
type Service struct {
	cfg     *project.Config
	dataDir string
	ai      AIConfig
	mu      sync.Mutex

	pmu      sync.Mutex
	previews map[string]*previewEntry

	rmu             sync.Mutex
	requests        *actionRequestState
	requestTimeout  time.Duration
	requestNotify   RequestReadyNotifier
	requestCancels  map[string]context.CancelFunc
	requestConfirms map[string]struct{}
}

// NewService builds a Service. dataDir is the fetcher output directory holding
// jobs/*.json and the *_state.json files.
func NewService(cfg *project.Config, dataDir string, ai AIConfig) *Service {
	s := &Service{
		cfg: cfg, dataDir: dataDir, ai: ai,
		previews: map[string]*previewEntry{}, requestCancels: map[string]context.CancelFunc{}, requestConfirms: map[string]struct{}{},
		requestTimeout: defaultRequestTimeout,
	}
	s.loadActionRequests()
	return s
}

// aiClient returns a chat client when AI is fully configured, else a nil
// pointer (callers must nil-check the concrete type). Both the template fillers
// and the revise step use it.
func (s *Service) aiClient() *ai.Client {
	if s.ai.Endpoint == "" || s.ai.Model == "" || s.ai.Token == "" {
		return nil
	}
	return ai.NewClientWithOptions(ai.Options{
		Token:        s.ai.Token,
		Endpoint:     s.ai.Endpoint,
		Model:        s.ai.Model,
		ExtraHeaders: s.ai.Headers,
	})
}

// aiCompleter returns an AI client for template reformatting when AI is fully
// configured, else nil (which makes the fillers a pass-through). It returns a
// true nil interface so callers' nil checks work.
func (s *Service) aiCompleter() repotemplate.Completer {
	c := s.aiClient()
	if c == nil {
		return nil
	}
	return c
}

// findPattern resolves a failure id to its PatternAnalysis by scanning the
// published per-job details.
func (s *Service) findPattern(id string) (*models.PatternAnalysis, error) {
	if strings.TrimSpace(id) == "" {
		return nil, ErrNotFound
	}
	jobsDir := filepath.Join(s.dataDir, "jobs")
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return nil, fmt.Errorf("reading job details: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(jobsDir, e.Name()))
		if err != nil {
			continue
		}
		var detail models.JobDetail
		if json.Unmarshal(data, &detail) != nil {
			continue
		}
		for i := range detail.PatternAnalyses {
			if detail.PatternAnalyses[i].ID != "" && detail.PatternAnalyses[i].ID == id {
				return &detail.PatternAnalyses[i], nil
			}
		}
	}
	return nil, ErrNotFound
}

// buildIssueSpec resolves the failure to a single issue spec and the target
// repo. It forces the patterns trigger: the admin explicitly asked to file
// this, so the project's configured triggers do not gate the on-demand action.
func (s *Service) buildIssueSpec(failureID string) (issues.IssueSpec, string, error) {
	pa, err := s.findPattern(failureID)
	if err != nil {
		return issues.IssueSpec{}, "", err
	}
	eff := s.cfg.EffectiveIssues()
	if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
		return issues.IssueSpec{}, "", fmt.Errorf("no target repo resolved (set issues.repo or branding.source_repo)")
	}
	report := models.FlakinessReport{RecurringPatterns: []models.PatternAnalysis{*pa}}
	specs := issues.BuildSpecs(issues.BuildInput{
		Report:       report,
		Triggers:     []string{project.IssueTriggerPatterns},
		Labels:       eff.Labels,
		DashboardURL: s.cfg.Branding.SiteURL,
	})
	if len(specs) == 0 {
		return issues.IssueSpec{}, "", fmt.Errorf("failure %s is not an actionable systemic pattern", failureID)
	}
	return specs[0], eff.Repo.Owner + "/" + eff.Repo.Name, nil
}

// buildFixManager builds the fix-PR manager for the source repo using
// userToken. It does not resolve a failure; callers that need the pattern look
// it up separately.
func (s *Service) buildFixManager(userToken string) (*fixpr.Manager, error) {
	eff := s.cfg.EffectiveFixPRs()
	if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
		return nil, fmt.Errorf("no source repo resolved (set ai.fix_prs.repo or branding.source_repo)")
	}
	aiClient := s.aiClient()
	ar := eff.AgentRuntime
	if aiClient == nil && ar.Type != "orka" {
		return nil, fmt.Errorf("AI is not configured on the server; cannot draft a local fix")
	}

	critique, critiqueRetries, err := fixruntime.Critique(aiClient, eff.CritiqueRetries)
	if err != nil {
		return nil, err
	}

	var prFiller fixpr.PRBodyFiller
	if aiClient != nil {
		prFiller = repotemplate.NewPRFiller(userToken, aiClient, eff.Repo.Owner, eff.Repo.Name)
	}
	prClient := fixpr.NewClients(userToken)
	opts := fixpr.Options{
		SourceOwner:     eff.Repo.Owner,
		SourceName:      eff.Repo.Name,
		Fork:            eff.Fork == nil || *eff.Fork,
		AuthorName:      eff.AuthorName,
		AuthorEmail:     eff.AuthorEmail,
		MaxFiles:        eff.MaxFiles,
		MaxNewPerRun:    1,
		Labels:          eff.Labels,
		DashboardURL:    s.cfg.Branding.SiteURL,
		Critique:        critique,
		CritiqueRetries: critiqueRetries,
		PRFiller:        prFiller,
	}
	if eff.Verify != nil && eff.Verify.Enabled {
		opts.Verify = &fixpr.VerifyConfig{
			Runtime:  runtime.NewLocal(),
			Commands: eff.Verify.ParsedCommands(),
			Timeout:  eff.Verify.ParsedTimeout(),
			Token:    userToken,
		}
	}
	allowBash := ar.AllowBash == nil || *ar.AllowBash
	agentRuntime, err := fixruntime.New(ar)
	if err != nil {
		return nil, err
	}
	model := ar.Model
	if model == "" {
		model = s.ai.Model
	}
	opts.Agent = &fixpr.AgentConfig{
		Runtime:    agentRuntime,
		Model:      model,
		Endpoint:   s.ai.Endpoint,
		ModelToken: s.ai.Token,
		MaxTurns:   ar.MaxTurns,
		AllowBash:  allowBash,
		Timeout:    ar.ParsedTimeout(),
		GitToken:   userToken,
	}
	mgr := fixpr.NewManager(prClient,
		filepath.Join(s.dataDir, "fix_pr_state.json"), opts)
	return mgr, nil
}

// generateIssuePreview renders an issue draft without caching or posting it.
func (s *Service) generateIssuePreview(ctx context.Context, failureID, userToken, instruction string) (PreviewResult, *previewEntry, error) {
	spec, _, err := s.buildIssueSpec(failureID)
	if err != nil {
		return PreviewResult{}, nil, err
	}
	var filler issues.TemplateFiller
	if c := s.aiCompleter(); c != nil {
		eff := s.cfg.EffectiveIssues()
		filler = repotemplate.NewIssueFiller(userToken, c, eff.Repo.Owner, eff.Repo.Name)
	}
	title, body := issues.RenderSpec(ctx, filler, spec)
	final := issues.IssueSpec{Key: spec.Key, Title: title, Body: body, Labels: spec.Labels}
	if strings.TrimSpace(instruction) != "" {
		if c := s.aiClient(); c != nil {
			final = issues.ReviseBody(ctx, c, final, instruction)
		}
	}
	return PreviewResult{Kind: "issue", Title: final.Title, Body: final.Body},
		&previewEntry{kind: "issue", spec: final}, nil
}

// PreviewIssue renders the exact issue that would be filed for the failure,
// without filing it, and caches it for confirmation.
func (s *Service) PreviewIssue(ctx context.Context, failureID, userToken, instruction string) (PreviewResult, error) {
	preview, entry, err := s.generateIssuePreview(ctx, failureID, userToken, instruction)
	if err != nil {
		return PreviewResult{}, err
	}
	token, err := s.stash(userToken, entry)
	if err != nil {
		return PreviewResult{}, err
	}
	preview.Token = token
	return preview, nil
}

// generateFixPreview creates a fix draft without caching or opening it.
func (s *Service) generateFixPreview(ctx context.Context, failureID, userToken, instruction string) (PreviewResult, *previewEntry, error) {
	pa, err := s.findPattern(failureID)
	if err != nil {
		return PreviewResult{}, nil, err
	}
	retry, priorPatchHash, retryInstruction, err := s.retryContext(failureID)
	if err != nil {
		return PreviewResult{}, nil, err
	}
	if retryInstruction != "" {
		instruction = strings.TrimSpace(retryInstruction + "\n\n" + instruction)
	}
	mgr, err := s.buildFixManager(userToken)
	if err != nil {
		return PreviewResult{}, nil, err
	}
	gf, err := mgr.GeneratePreview(ctx, *pa, instruction)
	if err != nil {
		return PreviewResult{}, nil, fmt.Errorf("%s", safeReason(err.Error()))
	}
	if retry && priorPatchHash != "" && fixpr.PatchHash(gf.Preview.Diff) == priorPatchHash {
		return PreviewResult{}, nil, fmt.Errorf("the follow-up produced the same patch as the failed attempt")
	}
	return PreviewResult{
		Kind: gfKind, Title: gf.Title, Body: gf.Description, Diff: gf.Preview.Diff,
		VerifyStatus: string(gf.Preview.Verify.Status), VerifySummary: gf.Preview.Verify.Summary,
		VerifyOutput: gf.Preview.Verify.Output,
	}, &previewEntry{kind: gfKind, fix: gf, retry: retry, failureID: failureID}, nil
}

const gfKind = "fix"

func (s *Service) retryContext(failureID string) (bool, string, string, error) {
	state := remediation.Load(s.dataDir)
	entry := state.Remediations[failureID]
	if entry == nil {
		for _, candidate := range state.Remediations {
			if candidate != nil && candidate.FindingID == failureID {
				entry = candidate
				break
			}
		}
	}
	if entry == nil || len(entry.Attempts) == 0 {
		return false, "", "", nil
	}
	latest := entry.Attempts[len(entry.Attempts)-1]
	if latest.Status != remediation.StatusStillFailingSameCause {
		return false, "", "", fmt.Errorf("an existing remediation is %s; a new fix requires a confirmed same-cause recurrence", latest.Status)
	}
	if len(entry.Attempts) >= 2 {
		return false, "", "", fmt.Errorf("the remediation retry limit has been reached")
	}
	if latest.PatchHash == "" {
		return false, "", "", fmt.Errorf("the prior patch fingerprint is unavailable; retry manually")
	}
	var builds []string
	for _, observation := range latest.Observations {
		if observation.Outcome == remediation.OutcomeSameCause {
			builds = append(builds, observation.BuildID)
		}
	}
	instruction := fmt.Sprintf("This is follow-up attempt %d. Pull request %s merged, but post-merge Prow evidence showed the same failure still exists. Previous patch hash: %s. Failed post-merge builds: %s. Previous outcome: %s. Do not repeat the previous approach; account for why it failed.",
		len(entry.Attempts)+1, latest.URL, latest.PatchHash, strings.Join(builds, ", "), latest.OutcomeReason)
	return true, latest.PatchHash, instruction, nil
}

// PreviewFix generates the exact fix PR preview and caches it for confirmation.
func (s *Service) PreviewFix(ctx context.Context, failureID, userToken, instruction string) (PreviewResult, error) {
	preview, entry, err := s.generateFixPreview(ctx, failureID, userToken, instruction)
	if err != nil {
		return PreviewResult{}, err
	}
	token, err := s.stash(userToken, entry)
	if err != nil {
		return PreviewResult{}, err
	}
	preview.Token = token
	return preview, nil
}

// Confirm files the issue or opens the PR previously cached under token.
func (s *Service) Confirm(ctx context.Context, token, userToken string) (string, error) {
	entry, err := s.take(userToken, token)
	if err != nil {
		return "", err
	}
	return s.confirmEntry(ctx, entry, userToken)
}

func (s *Service) confirmEntry(ctx context.Context, entry *previewEntry, userToken string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch entry.kind {
	case "issue":
		eff := s.cfg.EffectiveIssues()
		if eff.Repo == nil || eff.Repo.Owner == "" || eff.Repo.Name == "" {
			return "", fmt.Errorf("no target repo resolved (set issues.repo or branding.source_repo)")
		}
		client := issues.NewClient(userToken, eff.Repo.Owner, eff.Repo.Name)
		targetRepo := eff.Repo.Owner + "/" + eff.Repo.Name
		mgr := issues.NewManager(client, filepath.Join(s.dataDir, "issue_state.json"), targetRepo, issues.Options{MaxNewPerRun: 1})
		if _, err := mgr.Reconcile(ctx, []issues.IssueSpec{entry.spec}); err != nil {
			return "", fmt.Errorf("filing issue: %w", err)
		}
		if err := mgr.SaveState(); err != nil {
			return "", fmt.Errorf("saving issue state: %w", err)
		}
		url, ok := mgr.TrackedURL(entry.spec.Key)
		if !ok {
			return "", fmt.Errorf("issue was not filed")
		}
		return url, nil
	case gfKind:
		mgr, err := s.buildFixManager(userToken)
		if err != nil {
			return "", err
		}
		reservationID := ""
		if entry.retry {
			recoverPR := func(priorURL string) (string, bool, error) {
				return mgr.FindFollowUpPR(ctx, entry.fix, priorURL)
			}
			existingURL, id, err := s.reserveRetry(entry.failureID, fixpr.PatchHash(entry.fix.Preview.Diff), recoverPR)
			if err != nil {
				return "", err
			}
			if existingURL != "" {
				return existingURL, nil
			}
			reservationID = id
			mgr.ForgetGenerated(entry.fix)
		}
		url, err := mgr.OpenFromPreview(ctx, entry.fix)
		if err != nil {
			if reservationID != "" {
				s.clearRetryReservation(entry.failureID, reservationID)
			}
			return "", fmt.Errorf("%s", safeReason(err.Error()))
		}
		if reservationID != "" {
			if err := s.completeRetryReservation(entry.failureID, reservationID, url); err != nil {
				return "", err
			}
		}
		if err := mgr.SaveState(); err != nil {
			return "", fmt.Errorf("saving fix-PR state: %w", err)
		}
		return url, nil
	}
	return "", ErrPreviewNotFound
}

func (s *Service) reserveRetry(failureID, patchHash string, recoverPR func(string) (string, bool, error)) (string, string, error) {
	ledger := remediation.Load(s.dataDir)
	entry := remediationForFinding(ledger, failureID)
	if entry == nil || len(entry.Attempts) == 0 {
		return "", "", fmt.Errorf("remediation retry is no longer available")
	}
	latest := entry.Attempts[len(entry.Attempts)-1]
	reservationKey := retryReservationKey(entry, latest, failureID)
	state := s.loadRetryReservations()
	pruneRetryReservations(state, time.Now().UTC())
	if existing, ok := state.Reservations[reservationKey]; ok && existing.ResultURL != "" {
		return existing.ResultURL, existing.ID, nil
	}
	if resultURL, reservationID, found := completedReservationForEntry(state, entry); found {
		return resultURL, reservationID, nil
	}
	if latest.Status != remediation.StatusStillFailingSameCause || len(entry.Attempts) >= 2 {
		return "", "", fmt.Errorf("remediation retry is no longer available")
	}

	if existing, ok := state.Reservations[reservationKey]; ok {

		createdAt, err := time.Parse(time.RFC3339, existing.CreatedAt)
		if err == nil && time.Since(createdAt) <= retryReservationTTL {
			return "", "", fmt.Errorf("a remediation retry is already in progress")
		}
		if recoverPR != nil {
			recoveredURL, found, err := recoverPR(existing.PriorURL)
			if err != nil {
				return "", "", err
			}
			if found {
				existing.ResultURL = recoveredURL
				state.Reservations[reservationKey] = existing
				if err := s.saveRetryReservations(state); err != nil {
					return "", "", err
				}
				return recoveredURL, existing.ID, nil
			}
		}
		delete(state.Reservations, reservationKey)
		if err := s.saveRetryReservations(state); err != nil {
			return "", "", err
		}
	}

	id, err := newToken()
	if err != nil {
		return "", "", err
	}
	state.Reservations[reservationKey] = retryReservation{
		ID: id, PatchHash: patchHash, PriorURL: latest.URL,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.saveRetryReservations(state); err != nil {
		return "", "", err
	}
	return "", id, nil
}

func (s *Service) completeRetryReservation(failureID, reservationID, resultURL string) error {
	ledger := remediation.Load(s.dataDir)
	entry := remediationForFinding(ledger, failureID)
	if entry == nil {
		return fmt.Errorf("remediation retry reservation was lost")
	}
	latest := entry.Attempts[len(entry.Attempts)-1]
	reservationKey := retryReservationKey(entry, latest, failureID)
	state := s.loadRetryReservations()
	reservation, ok := state.Reservations[reservationKey]
	if !ok || reservation.ID != reservationID {
		return fmt.Errorf("remediation retry reservation was lost")
	}
	reservation.ResultURL = resultURL
	state.Reservations[reservationKey] = reservation
	return s.saveRetryReservations(state)
}

func (s *Service) clearRetryReservation(failureID, reservationID string) {
	ledger := remediation.Load(s.dataDir)
	entry := remediationForFinding(ledger, failureID)
	if entry == nil {
		return
	}
	latest := entry.Attempts[len(entry.Attempts)-1]
	reservationKey := retryReservationKey(entry, latest, failureID)
	state := s.loadRetryReservations()
	reservation, ok := state.Reservations[reservationKey]
	if !ok || reservation.ID != reservationID || reservation.ResultURL != "" {
		return
	}
	delete(state.Reservations, reservationKey)
	_ = s.saveRetryReservations(state)
}

func completedReservationForEntry(state *retryReservationState, entry *remediation.Remediation) (string, string, bool) {
	if state == nil || entry == nil {
		return "", "", false
	}
	attemptURLs := map[string]bool{}
	for _, attempt := range entry.Attempts {
		if attempt.URL != "" {
			attemptURLs[attempt.URL] = true
		}
	}
	prefix := entry.ID + "::"
	for key, reservation := range state.Reservations {
		if strings.HasPrefix(key, prefix) && reservation.ResultURL != "" && attemptURLs[reservation.ResultURL] {
			return reservation.ResultURL, reservation.ID, true
		}
	}
	return "", "", false
}

func retryReservationKey(entry *remediation.Remediation, attempt remediation.Attempt, fallback string) string {
	stable := entry.ID
	if stable == "" {
		stable = fallback
	}
	identity := attempt.URL
	if identity == "" {
		identity = fmt.Sprintf("attempt-%d", attempt.Number)
	}
	return stable + "::" + identity
}

func pruneRetryReservations(state *retryReservationState, now time.Time) {
	const retention = 180 * 24 * time.Hour
	for key, reservation := range state.Reservations {
		created, err := time.Parse(time.RFC3339, reservation.CreatedAt)
		if err != nil || now.Sub(created) > retention {
			delete(state.Reservations, key)
		}
	}
}

func remediationForFinding(state *remediation.State, failureID string) *remediation.Remediation {
	if state == nil {
		return nil
	}
	if entry := state.Remediations[failureID]; entry != nil {
		return entry
	}
	for _, entry := range state.Remediations {
		if entry != nil && entry.FindingID == failureID {
			return entry
		}
	}
	return nil
}

// Resolve marks a systemic pattern as resolved: it is hidden from the active
// view until a failing build newer than the current watermark recurs. note is
// an optional maintainer comment (e.g. the fixing PR). login attributes it.
func (s *Service) Resolve(failureID, login, note string) error {
	pa, err := s.findPattern(failureID)
	if err != nil {
		return err
	}
	if !pa.Systemic {
		return fmt.Errorf("only systemic recurring patterns can be resolved")
	}
	watermark := resolve.Watermark(*pa)
	if watermark == "" {
		return fmt.Errorf("pattern has no build history to resolve against")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	st := resolve.Load(s.dataDir)
	st.Resolved[failureID] = resolve.Entry{
		ResolvedAt: time.Now().UTC().Format(time.RFC3339),
		ResolvedBy: login,
		Note:       strings.TrimSpace(note),
		Watermark:  watermark,
		Subject:    pa.Subject,
	}
	if err := st.Save(s.dataDir); err != nil {
		return fmt.Errorf("saving resolved state: %w", err)
	}
	return nil
}

// Unresolve clears a pattern's resolved mark so it returns to the active view.
func (s *Service) Unresolve(failureID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := resolve.Load(s.dataDir)
	if !st.IsResolved(failureID) {
		return ErrNotFound
	}
	delete(st.Resolved, failureID)
	if err := st.Save(s.dataDir); err != nil {
		return fmt.Errorf("saving resolved state: %w", err)
	}
	return nil
}

// stash caches a draft under a fresh token bound to the admin's identity and
// returns the token, evicting expired entries first.
func (s *Service) stash(userToken string, e *previewEntry) (string, error) {
	e.owner = tokenHash(userToken)
	e.createdAt = time.Now()
	token, err := newToken()
	if err != nil {
		return "", fmt.Errorf("generating preview token: %w", err)
	}
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.evictExpiredLocked()
	s.previews[token] = e
	return token, nil
}

// take removes and returns the draft cached under token if it exists, has not
// expired, and belongs to the same admin.
func (s *Service) take(userToken, token string) (*previewEntry, error) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.evictExpiredLocked()
	e, ok := s.previews[token]
	if !ok || e.owner != tokenHash(userToken) {
		return nil, ErrPreviewNotFound
	}
	delete(s.previews, token)
	return e, nil
}

func (s *Service) evictExpiredLocked() {
	cutoff := time.Now().Add(-previewTTL)
	for k, e := range s.previews {
		if e.createdAt.Before(cutoff) {
			delete(s.previews, k)
		}
	}
}

// tokenHash binds a preview to the admin who generated it without retaining the
// raw token.
func tokenHash(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// safeReason turns an internal failure reason into a message safe to show a
// user. Reasons from the AI provider (which may echo an opaque response body)
// are replaced with a generic message; our own pipeline messages pass through,
// truncated. It never exposes endpoints, tokens, or provider bodies.
func safeReason(reason string) string {
	reason = strings.TrimSpace(reason)
	low := strings.ToLower(reason)
	// AI transport/provider errors: do not leak the provider's response body.
	if strings.Contains(low, "chat returned") || strings.Contains(low, "unauthorized") ||
		strings.Contains(low, "status code") || strings.Contains(low, "http ") {
		return "the AI service could not complete the request"
	}
	const max = 300
	if len(reason) > max {
		reason = reason[:max] + "…"
	}
	if reason == "" {
		return "the fix could not be generated"
	}
	return reason
}
