package skillsuggest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

var errLabel = errors.New("labeling failed")

const validRecipe = `id: etcd-join-timeout
name: Etcd join timeout
triggers:
  - "(?i)etcd.*timeout"
required_evidence:
  - id: kubelet-log
    any_of:
      - "artifacts/.*/kubelet.log"
procedure: |
  1. Read kubelet.log and cite the etcd timeout.
`

// fakeCompleter routes by the system prompt: covered-check vs recipe generation.
type fakeCompleter struct {
	covered      string // JSON returned for the covered-check
	recipe       string // YAML returned for generation
	coveredErr   error
	recipeErr    error
	coveredCalls int
	recipeCalls  int
}

func (f *fakeCompleter) Complete(_ context.Context, system, _ string) (string, error) {
	if system == coveredSystemPrompt {
		f.coveredCalls++
		return f.covered, f.coveredErr
	}
	f.recipeCalls++
	return f.recipe, f.recipeErr
}

// fakePR records OpenPR calls and serves a configurable SearchOpenPR result.
type fakePR struct {
	opened      []ghpr.Request
	openErr     error
	openURL     string // url returned alongside openErr (partial success)
	searchURL   string
	searchFound bool
	searchErr   error
}

func (f *fakePR) OpenPR(_ context.Context, req ghpr.Request) (string, error) {
	if f.openErr != nil {
		f.opened = append(f.opened, req) // a PR with openURL set means it opened
		return f.openURL, f.openErr
	}
	f.opened = append(f.opened, req)
	return "https://github.com/o/r/pull/9", nil
}

func (f *fakePR) SearchOpenPR(_ context.Context, _, _, _, _ string) (int, string, bool, error) {
	if f.searchErr != nil {
		return 0, "", false, f.searchErr
	}
	if f.searchFound {
		return 9, f.searchURL, true, nil
	}
	return 0, "", false, nil
}

func systemicPattern(subject string) models.PatternAnalysis {
	return models.PatternAnalysis{
		Subject:         subject,
		JobID:           "job-" + subject,
		Systemic:        true,
		Confidence:      "high",
		SharedRootCause: "etcd member join timed out on the second control plane node",
		Summary:         "Most builds fail joining etcd.",
		BuildsAnalyzed:  5,
	}
}

func newManager(t *testing.T, pr prClient, c Completer, existing *skills.Set, opts Options) *Manager {
	t.Helper()
	if opts.MinConfidence == "" {
		opts.MinConfidence = "high"
	}
	if opts.MaxNewPerRun == 0 {
		opts.MaxNewPerRun = 1
	}
	state := filepath.Join(t.TempDir(), "state.json")
	return NewManager(pr, c, existing, "o", "r", state, opts)
}

func TestReconcile_SuggestsForUncoveredPattern(t *testing.T) {
	pr := &fakePR{}
	c := &fakeCompleter{covered: `{"covered": false}`, recipe: validRecipe}
	m := newManager(t, pr, c, nil, Options{})

	stats, err := m.Reconcile(context.Background(), []models.PatternAnalysis{systemicPattern("etcd")})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Suggested != 1 {
		t.Fatalf("Suggested = %d, want 1", stats.Suggested)
	}
	if len(pr.opened) != 1 {
		t.Fatalf("opened %d PRs, want 1", len(pr.opened))
	}
	got := pr.opened[0]
	if _, ok := got.Files["skills/etcd-join-timeout.yaml"]; !ok {
		t.Errorf("PR files = %v, want skills/etcd-join-timeout.yaml", got.Files)
	}
	if !got.Draft {
		t.Errorf("suggestion PR should be a draft")
	}
	if !strings.Contains(got.Body, "prow-ai-dashboard-skill:") {
		t.Errorf("PR body missing dedup marker: %q", got.Body)
	}
	// With no existing skills there's nothing to cover, so the LLM covered-check
	// is skipped.
	if c.coveredCalls != 0 {
		t.Errorf("coveredCalls = %d, want 0 (no existing skills)", c.coveredCalls)
	}
}

func TestReconcile_SkipsAlreadyTracked(t *testing.T) {
	pr := &fakePR{}
	c := &fakeCompleter{covered: `{"covered": false}`, recipe: validRecipe}
	m := newManager(t, pr, c, nil, Options{})
	p := systemicPattern("etcd")
	m.state.Tracked[keyFor(p)] = TrackedPR{URL: "x", OpenedAt: now()}

	stats, err := m.Reconcile(context.Background(), []models.PatternAnalysis{p})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Suggested != 0 || len(pr.opened) != 0 {
		t.Errorf("expected no new PR for a tracked pattern, got %+v / %d", stats, len(pr.opened))
	}
}

func TestReconcile_AdoptsOpenPR(t *testing.T) {
	pr := &fakePR{searchFound: true, searchURL: "https://github.com/o/r/pull/3"}
	c := &fakeCompleter{covered: `{"covered": false}`, recipe: validRecipe}
	m := newManager(t, pr, c, nil, Options{})

	stats, err := m.Reconcile(context.Background(), []models.PatternAnalysis{systemicPattern("etcd")})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Adopted != 1 || stats.Suggested != 0 {
		t.Errorf("stats = %+v, want Adopted 1 / Suggested 0", stats)
	}
	if len(pr.opened) != 0 {
		t.Errorf("should not open a new PR when one is already open")
	}
}

func TestReconcile_SkipsCovered(t *testing.T) {
	existing := loadSkills(t, map[string]string{"x.yaml": validRecipe})
	pr := &fakePR{}
	c := &fakeCompleter{covered: `{"covered": true, "skill_id": "etcd-join-timeout"}`, recipe: validRecipe}
	m := newManager(t, pr, c, existing, Options{})

	stats, err := m.Reconcile(context.Background(), []models.PatternAnalysis{systemicPattern("etcd")})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Covered != 1 || stats.Suggested != 0 || len(pr.opened) != 0 {
		t.Errorf("stats = %+v / opened %d, want Covered 1 and no PR", stats, len(pr.opened))
	}
}

func TestReconcile_InvalidRecipeSkipped(t *testing.T) {
	pr := &fakePR{}
	c := &fakeCompleter{covered: `{"covered": false}`, recipe: "id: x\n# no triggers -> invalid\n"}
	m := newManager(t, pr, c, nil, Options{})

	stats, err := m.Reconcile(context.Background(), []models.PatternAnalysis{systemicPattern("etcd")})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Suggested != 0 || len(pr.opened) != 0 {
		t.Errorf("invalid recipe should not open a PR, got %+v / %d", stats, len(pr.opened))
	}
}

func TestReconcile_FiltersIneligibleAndCap(t *testing.T) {
	pr := &fakePR{}
	c := &fakeCompleter{covered: `{"covered": false}`, recipe: validRecipe}
	m := newManager(t, pr, c, nil, Options{MaxNewPerRun: 1})

	notSystemic := systemicPattern("flaky")
	notSystemic.Systemic = false
	lowConf := systemicPattern("low")
	lowConf.Confidence = "low"
	noCause := systemicPattern("nocause")
	noCause.SharedRootCause = ""
	good1 := systemicPattern("etcd")
	good2 := systemicPattern("webhook")
	good2.SharedRootCause = "aso webhook not ready"

	stats, err := m.Reconcile(context.Background(),
		[]models.PatternAnalysis{notSystemic, lowConf, noCause, good1, good2})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// Two eligible patterns but the cap is 1.
	if stats.Suggested != 1 || len(pr.opened) != 1 {
		t.Errorf("expected exactly 1 suggestion (cap), got %+v / %d", stats, len(pr.opened))
	}
}

func TestGenerateRecipe_IDCollision(t *testing.T) {
	c := &fakeCompleter{recipe: validRecipe}
	id, content, err := generateRecipe(context.Background(), c, systemicPattern("etcd"), []string{"etcd-join-timeout"})
	if err != nil {
		t.Fatalf("generateRecipe: %v", err)
	}
	if id != "etcd-join-timeout-2" {
		t.Errorf("id = %q, want collision-suffixed etcd-join-timeout-2", id)
	}
	if !strings.Contains(content, "id: etcd-join-timeout-2") {
		t.Errorf("recipe id line not re-stamped: %q", content)
	}
}

func TestExtractYAML_StripsFence(t *testing.T) {
	got := extractYAML("```yaml\nid: x\n```")
	if got != "id: x" {
		t.Errorf("extractYAML = %q", got)
	}
}

func TestReconcile_PartialSuccessTracksAndCounts(t *testing.T) {
	// OpenPR returns a url plus an error (e.g. labeling failed). The PR exists,
	// so it must be tracked and counted against the cap (not retried/duplicated).
	pr := &fakePR{openErr: errLabel, openURL: "https://github.com/o/r/pull/9"}
	c := &fakeCompleter{covered: `{"covered": false}`, recipe: validRecipe}
	m := newManager(t, pr, c, nil, Options{})
	p := systemicPattern("etcd")

	stats, err := m.Reconcile(context.Background(), []models.PatternAnalysis{p})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Suggested != 1 {
		t.Errorf("Suggested = %d, want 1 (partial success still counts)", stats.Suggested)
	}
	if _, tracked := m.state.Tracked[keyFor(p)]; !tracked {
		t.Errorf("pattern should be tracked after a partial-success PR")
	}
}

func TestSanitizeID(t *testing.T) {
	cases := map[string]string{
		"Etcd Join Timeout": "etcd-join-timeout",
		"a/../b":            "a-b",
		"foo/bar":           "foo-bar",
		"--weird__id--":     "weird-id",
		"???":               "",
	}
	for in, want := range cases {
		if got := sanitizeID(in); got != want {
			t.Errorf("sanitizeID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGenerateRecipe_SanitizesUnsafeID(t *testing.T) {
	// A model that emits an unsafe id must yield a safe kebab filename.
	c := &fakeCompleter{recipe: strings.Replace(validRecipe, "id: etcd-join-timeout", "id: Etcd/Join Timeout", 1)}
	id, content, err := generateRecipe(context.Background(), c, systemicPattern("etcd"), nil)
	if err != nil {
		t.Fatalf("generateRecipe: %v", err)
	}
	if id != "etcd-join-timeout" {
		t.Errorf("id = %q, want sanitized etcd-join-timeout", id)
	}
	if !strings.Contains(content, "id: etcd-join-timeout") {
		t.Errorf("recipe id line not re-stamped to safe id: %q", content)
	}
}

// loadSkills writes recipe files to a temp dir and loads them into a Set.
func loadSkills(t *testing.T, files map[string]string) *skills.Set {
	t.Helper()
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(skillsDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	set, err := skills.Load(dir)
	if err != nil {
		t.Fatalf("loading skills: %v", err)
	}
	return set
}
