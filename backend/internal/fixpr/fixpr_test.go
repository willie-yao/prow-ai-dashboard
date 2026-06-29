package fixpr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// fakeCompleter routes by system prompt: locate vs edit.
type fakeCompleter struct {
	locate    string
	edit      string
	locateErr error
	editErr   error
}

func (f *fakeCompleter) Complete(_ context.Context, system, _ string) (string, error) {
	if system == locateSystemPrompt {
		return f.locate, f.locateErr
	}
	return f.edit, f.editErr
}

// fakeSource serves canned file content and records the ref it was read at.
type fakeSource struct {
	files   map[string]string
	lastRef string
}

func (s *fakeSource) FileContent(_ context.Context, _, _, ref, path string) (string, bool, error) {
	s.lastRef = ref
	c, ok := s.files[path]
	return c, ok, nil
}

func (s *fakeSource) ListTree(_ context.Context, _, _, _ string) ([]string, error) {
	paths := make([]string, 0, len(s.files))
	for p := range s.files {
		paths = append(paths, p)
	}
	return paths, nil
}

// fakePR records OpenPR calls and serves a configurable SearchOpenPR result.
type fakePR struct {
	opened      []ghpr.Request
	openErr     error
	openURL     string
	searchURL   string
	searchFound bool
}

func (f *fakePR) OpenPR(_ context.Context, req ghpr.Request) (string, error) {
	f.opened = append(f.opened, req)
	if f.openErr != nil {
		return f.openURL, f.openErr
	}
	return "https://github.com/up/stream/pull/5", nil
}

func (f *fakePR) SearchOpenPR(_ context.Context, _, _, _, _ string) (int, string, bool, error) {
	if f.searchFound {
		return 5, f.searchURL, true, nil
	}
	return 0, "", false, nil
}

func (f *fakePR) ResolveBase(_ context.Context, _, _ string) (ghpr.Base, error) {
	return ghpr.Base{Branch: "main", HeadSHA: "pinned-sha-123", TreeSHA: "basetree"}, nil
}

const sampleFile = `apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster
spec:
  machineType: Standard_D2s_v3
  diskType: StandardSSD_LRS
`

func systemicPattern(subject string) models.PatternAnalysis {
	return models.PatternAnalysis{
		Subject:         subject,
		JobID:           "job-" + subject,
		Systemic:        true,
		Confidence:      "high",
		SharedRootCause: "etcd disk too slow on StandardSSD_LRS causing join timeouts",
		SuggestedFix:    "pin the control plane disk to Premium_LRS",
		Summary:         "Most builds fail joining etcd.",
		BuildsAnalyzed:  5,
	}
}

// ---- generation ----

func TestGenerateFix_HappyPath(t *testing.T) {
	c := &fakeCompleter{
		locate: `{"files": ["templates/cluster.yaml"]}`,
		edit:   `{"rationale": "use a faster disk", "edits": [{"file": "templates/cluster.yaml", "old": "diskType: StandardSSD_LRS", "new": "diskType: Premium_LRS"}]}`,
	}
	src := &fakeSource{files: map[string]string{"templates/cluster.yaml": sampleFile}}

	fix, err := generateFix(context.Background(), c, src, "o", "r", "ref", systemicPattern("etcd"), 2)
	if err != nil {
		t.Fatalf("generateFix: %v", err)
	}
	got := fix.files["templates/cluster.yaml"]
	if !strings.Contains(got, "diskType: Premium_LRS") || strings.Contains(got, "StandardSSD_LRS") {
		t.Errorf("edit not applied: %q", got)
	}
	if !strings.Contains(fix.diff, "- ") || !strings.Contains(fix.diff, "+ ") {
		t.Errorf("diff not rendered: %q", fix.diff)
	}
}

func TestGenerateFix_RejectsHallucinatedFile(t *testing.T) {
	// The model picks a path that is not among the real candidate files.
	c := &fakeCompleter{locate: `{"files": ["does/not/exist.yaml"]}`}
	src := &fakeSource{files: map[string]string{"templates/cluster.yaml": sampleFile}}
	if _, err := generateFix(context.Background(), c, src, "o", "r", "ref", systemicPattern("etcd"), 2); err == nil || !strings.Contains(err.Error(), "not a real repo file") {
		t.Errorf("expected hallucinated-file rejection, got %v", err)
	}
}

func TestGenerateFix_NoCandidates(t *testing.T) {
	c := &fakeCompleter{}
	src := &fakeSource{files: map[string]string{}}
	if _, err := generateFix(context.Background(), c, src, "o", "r", "ref", systemicPattern("etcd"), 2); err == nil || !strings.Contains(err.Error(), "no candidate") {
		t.Errorf("expected no-candidate error, got %v", err)
	}
}

func TestGenerateFix_RejectsTooManyFiles(t *testing.T) {
	c := &fakeCompleter{locate: `{"files": ["templates/a.yaml", "templates/b.yaml", "templates/c.yaml"]}`}
	src := &fakeSource{files: map[string]string{
		"templates/a.yaml": "x", "templates/b.yaml": "y", "templates/c.yaml": "z",
	}}
	if _, err := generateFix(context.Background(), c, src, "o", "r", "ref", systemicPattern("etcd"), 2); err == nil || !strings.Contains(err.Error(), "max_files") {
		t.Errorf("expected max_files error, got %v", err)
	}
}

func TestRankCandidates_FiltersAndPrefers(t *testing.T) {
	tree := []string{
		"templates/test/ci/cluster-template-prow-azl3.yaml", // preferred dir + ext
		"vendor/foo/bar.go",      // no signal -> excluded
		"docs/proposals/etcd.md", // keyword but penalized dir
		"README.md",              // no signal
	}
	got := rankCandidates(tree, systemicPattern("etcd"))
	if len(got) == 0 || got[0] != "templates/test/ci/cluster-template-prow-azl3.yaml" {
		t.Errorf("expected the template path ranked first, got %v", got)
	}
	for _, p := range got {
		if strings.HasPrefix(p, "vendor/") {
			t.Errorf("vendor path should be filtered out: %v", got)
		}
	}
}

func TestRankCandidates_ExtensionAloneExcluded(t *testing.T) {
	// Paths with a matching extension but no keyword and no preferred dir must
	// not be admitted, so a weak keyword set can't flood the candidate list with
	// arbitrary files.
	tree := []string{"random/unrelated/file.go", "another/thing.yaml"}
	if got := rankCandidates(tree, systemicPattern("etcd")); len(got) != 0 {
		t.Errorf("extension-only paths should be excluded, got %v", got)
	}
}

func TestValidateSyntax(t *testing.T) {
	ok := []map[string]string{
		{"a.yaml": "key: val\n"},
		{"a.yaml": "a: 1\n---\nb: 2\n"}, // multi-doc
		{"a.go": "package x\n\nfunc F() {}\n"},
		{"a.json": `{"k": 1}`},
		{"a.sh": "this is $(not validated"}, // no validator -> skipped
	}
	for _, f := range ok {
		if err := validateSyntax(f); err != nil {
			t.Errorf("validateSyntax(%v) unexpected error: %v", f, err)
		}
	}
	bad := []struct{ file, content, want string }{
		{"a.yaml", "diskType: [unclosed\n", "not valid YAML"},
		{"a.go", "package x\nfunc F( {}\n", "not valid Go"},
		{"a.json", `{"k": }`, "not valid JSON"},
	}
	for _, c := range bad {
		err := validateSyntax(map[string]string{c.file: c.content})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("validateSyntax(%s) = %v, want %q", c.file, err, c.want)
		}
	}
}

func TestGenerateFix_DropsBrokenSyntax(t *testing.T) {
	// The edit turns valid YAML into an unclosed flow sequence.
	c := &fakeCompleter{
		locate: `{"files": ["templates/cluster.yaml"]}`,
		edit:   `{"rationale": "break it", "edits": [{"file": "templates/cluster.yaml", "old": "diskType: StandardSSD_LRS", "new": "diskType: [unclosed"}]}`,
	}
	src := &fakeSource{files: map[string]string{"templates/cluster.yaml": sampleFile}}
	if _, err := generateFix(context.Background(), c, src, "o", "r", "ref", systemicPattern("etcd"), 2); err == nil || !strings.Contains(err.Error(), "not valid YAML") {
		t.Errorf("expected broken-YAML drop, got %v", err)
	}
}

func TestApplyEdits_AnchorNotFound(t *testing.T) {
	_, err := applyEdits(map[string]string{"f": "hello"}, []edit{{File: "f", Old: "absent", New: "x"}}, 2)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected anchor-not-found, got %v", err)
	}
}

func TestApplyEdits_AmbiguousAnchor(t *testing.T) {
	_, err := applyEdits(map[string]string{"f": "x x"}, []edit{{File: "f", Old: "x", New: "y"}}, 2)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("expected ambiguous-anchor, got %v", err)
	}
}

func TestApplyEdits_NoEffectiveChange(t *testing.T) {
	_, err := applyEdits(map[string]string{"f": "a"}, []edit{{File: "f", Old: "a", New: "a"}}, 2)
	if err == nil || !strings.Contains(err.Error(), "no effective change") {
		t.Errorf("expected no-effective-change, got %v", err)
	}
}

func TestApplyEdits_OnlyReturnsChanged(t *testing.T) {
	orig := map[string]string{"a": "line1\nline2\nline3\n", "b": "two"}
	out, err := applyEdits(orig, []edit{{File: "a", Old: "line2", New: "changed"}}, 2)
	if err != nil {
		t.Fatalf("applyEdits: %v", err)
	}
	if len(out) != 1 || !strings.Contains(out["a"], "changed") {
		t.Errorf("expected only changed file a, got %v", out)
	}
}

func TestApplyEdits_RejectsWholeFileRewrite(t *testing.T) {
	_, err := applyEdits(map[string]string{"f": "the entire file"}, []edit{{File: "f", Old: "the entire file", New: "totally new"}}, 2)
	if err == nil || !strings.Contains(err.Error(), "whole file") {
		t.Errorf("expected whole-file-rewrite rejection, got %v", err)
	}
}

func TestApplyEdits_RejectsOversizedChange(t *testing.T) {
	big := strings.Repeat("newline\n", maxChangedLinesTotal+5)
	orig := "anchor-here\n" + strings.Repeat("x\n", 200)
	_, err := applyEdits(map[string]string{"f": orig}, []edit{{File: "f", Old: "anchor-here", New: big}}, 2)
	if err == nil || !strings.Contains(err.Error(), "exceeds the cap") {
		t.Errorf("expected oversized-change rejection, got %v", err)
	}
}

// ---- reconciler ----

func newManager(t *testing.T, pr prClient, c Completer, src sourceReader, opts Options) *Manager {
	t.Helper()
	opts.SourceOwner, opts.SourceName = "up", "stream"
	if opts.MinConfidence == "" {
		opts.MinConfidence = "high"
	}
	if opts.MaxFiles == 0 {
		opts.MaxFiles = 2
	}
	if opts.MaxNewPerRun == 0 {
		opts.MaxNewPerRun = 1
	}
	if opts.AuthorName == "" {
		opts.AuthorName, opts.AuthorEmail = "Jane", "jane@example.com"
	}
	// Default to fork-and-PR; tests can flip m.opts.Fork for direct mode.
	opts.Fork = true
	return NewManager(pr, c, src, filepath.Join(t.TempDir(), "state.json"), opts)
}

func goodCompleter() *fakeCompleter {
	return &fakeCompleter{
		locate: `{"files": ["templates/cluster.yaml"]}`,
		edit:   `{"rationale": "faster disk", "edits": [{"file": "templates/cluster.yaml", "old": "diskType: StandardSSD_LRS", "new": "diskType: Premium_LRS"}]}`,
	}
}

func goodSource() *fakeSource {
	return &fakeSource{files: map[string]string{"templates/cluster.yaml": sampleFile}}
}

func TestReconcile_DirectModeWhenForkFalse(t *testing.T) {
	pr := &fakePR{}
	m := newManager(t, pr, goodCompleter(), goodSource(), Options{})
	m.opts.Fork = false // direct branch + same-repo PR (source repo you own)
	if _, err := m.Reconcile(context.Background(), []models.PatternAnalysis{systemicPattern("etcd")}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(pr.opened) != 1 {
		t.Fatalf("expected 1 PR, got %d", len(pr.opened))
	}
	req := pr.opened[0]
	if req.Fork {
		t.Errorf("direct mode must not fork")
	}
	if !req.Draft || !req.SignOff {
		t.Errorf("fix PR should still be draft + signoff: %+v", req)
	}
}

func TestReconcile_OpensDraftForkPR(t *testing.T) {
	pr := &fakePR{}
	m := newManager(t, pr, goodCompleter(), goodSource(), Options{})
	stats, err := m.Reconcile(context.Background(), []models.PatternAnalysis{systemicPattern("etcd")})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Proposed != 1 || len(pr.opened) != 1 {
		t.Fatalf("stats=%+v opened=%d, want 1 proposed", stats, len(pr.opened))
	}
	req := pr.opened[0]
	if !req.Fork || !req.Draft || !req.SignOff {
		t.Errorf("fix PR must be fork+draft+signoff: %+v", req)
	}
	if req.Owner != "up" || req.Repo != "stream" {
		t.Errorf("PR target = %s/%s, want up/stream", req.Owner, req.Repo)
	}
	if req.AuthorName != "Jane" || req.AuthorEmail != "jane@example.com" {
		t.Errorf("author = %s <%s>", req.AuthorName, req.AuthorEmail)
	}
	if !strings.Contains(req.Body, "prow-ai-dashboard-fix:") {
		t.Errorf("PR body missing dedup marker")
	}
}

func TestReconcile_DryRunWritesPreviewsNoPR(t *testing.T) {
	pr := &fakePR{}
	previewFile := filepath.Join(t.TempDir(), "fix_previews.json")
	m := newManager(t, pr, goodCompleter(), goodSource(), Options{DryRun: true, PreviewFile: previewFile})
	stats, err := m.Reconcile(context.Background(), []models.PatternAnalysis{systemicPattern("etcd")})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Previewed != 1 || stats.Proposed != 0 {
		t.Errorf("stats=%+v, want 1 previewed 0 proposed", stats)
	}
	if len(pr.opened) != 0 {
		t.Errorf("dry-run must not open a PR")
	}
	if _, err := os.Stat(previewFile); err != nil {
		t.Errorf("previews file not written: %v", err)
	}
}

func TestReconcile_SkipsTrackedAndAdoptsOpen(t *testing.T) {
	// Tracked: no PR.
	pr := &fakePR{}
	m := newManager(t, pr, goodCompleter(), goodSource(), Options{})
	p := systemicPattern("etcd")
	m.state.Tracked[keyFor(p)] = TrackedFix{URL: "x", OpenedAt: now()}
	stats, _ := m.Reconcile(context.Background(), []models.PatternAnalysis{p})
	if stats.Proposed != 0 || len(pr.opened) != 0 {
		t.Errorf("tracked pattern should be skipped: %+v", stats)
	}

	// Open PR found via search: adopt, no new PR.
	pr2 := &fakePR{searchFound: true, searchURL: "https://github.com/up/stream/pull/3"}
	m2 := newManager(t, pr2, goodCompleter(), goodSource(), Options{})
	stats2, _ := m2.Reconcile(context.Background(), []models.PatternAnalysis{systemicPattern("etcd")})
	if stats2.Adopted != 1 || len(pr2.opened) != 0 {
		t.Errorf("expected adopt without opening: %+v", stats2)
	}
}

func TestReconcile_FiltersIneligibleAndCap(t *testing.T) {
	pr := &fakePR{}
	m := newManager(t, pr, goodCompleter(), goodSource(), Options{MaxNewPerRun: 1})

	notSystemic := systemicPattern("a")
	notSystemic.Systemic = false
	noFix := systemicPattern("b")
	noFix.SuggestedFix = ""
	lowConf := systemicPattern("c")
	lowConf.Confidence = "low"
	good1 := systemicPattern("etcd")
	good2 := systemicPattern("webhook")
	good2.SharedRootCause = "different cause"

	stats, _ := m.Reconcile(context.Background(), []models.PatternAnalysis{notSystemic, noFix, lowConf, good1, good2})
	if stats.Proposed != 1 || len(pr.opened) != 1 {
		t.Errorf("expected exactly 1 proposal (cap), got %+v / %d", stats, len(pr.opened))
	}
}

func TestReconcile_PinsBaseAcrossReadAndCommit(t *testing.T) {
	pr := &fakePR{}
	src := goodSource()
	m := newManager(t, pr, goodCompleter(), src, Options{})
	if _, err := m.Reconcile(context.Background(), []models.PatternAnalysis{systemicPattern("etcd")}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The file was read at the pinned base SHA, and OpenPR received that same base.
	if src.lastRef != "pinned-sha-123" {
		t.Errorf("file read at ref %q, want pinned-sha-123", src.lastRef)
	}
	if len(pr.opened) != 1 || pr.opened[0].Base == nil || pr.opened[0].Base.HeadSHA != "pinned-sha-123" {
		t.Errorf("OpenPR base = %+v, want HeadSHA pinned-sha-123", pr.opened[0].Base)
	}
}

func TestReconcile_PartialSuccessTracksAndCounts(t *testing.T) {
	pr := &fakePR{openErr: errors.New("labeling failed"), openURL: "https://github.com/up/stream/pull/9"}
	m := newManager(t, pr, goodCompleter(), goodSource(), Options{})
	p := systemicPattern("etcd")
	stats, err := m.Reconcile(context.Background(), []models.PatternAnalysis{p})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Proposed != 1 {
		t.Errorf("partial success should count: %+v", stats)
	}
	if _, tracked := m.state.Tracked[keyFor(p)]; !tracked {
		t.Errorf("partial-success PR should be tracked")
	}
}
