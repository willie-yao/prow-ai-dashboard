package onboard

import (
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// ---------- category inference ----------

func TestInferCategories_GroupsAndOrders(t *testing.T) {
	jobs := []string{
		"periodic-capz-e2e-aks-main",
		"periodic-capz-e2e-aks-release-1-23",
		"periodic-capz-conformance-main",
		"periodic-capz-conformance-release-1-23",
		"periodic-capz-e2e-main",
		"periodic-capz-e2e-release-1-23",
		"periodic-capz-capi-e2e-main",
		"periodic-capz-capi-e2e-release-1-23",
	}
	rules := InferCategories(jobs)
	if len(rules) == 0 {
		t.Fatal("expected some categories")
	}

	ids := map[string]int{} // id -> position
	for i, r := range rules {
		ids[r.ID] = i
		// id and match are the bare token; label is human-cased.
		if r.Match != r.ID {
			t.Errorf("rule %q: match %q != id %q", r.ID, r.Match, r.ID)
		}
	}

	// "aks", "conformance", "capi" each group >=2 jobs; all should appear.
	for _, want := range []string{"aks", "conformance", "capi"} {
		if _, ok := ids[want]; !ok {
			t.Errorf("expected a %q category, got %v", want, ids)
		}
	}
	// Specific-before-broad: "aks"/"conformance"/"capi" (2 jobs each) precede
	// the broad "e2e" (which spans most jobs).
	if pos, ok := ids["e2e"]; ok {
		for _, narrow := range []string{"aks", "conformance", "capi"} {
			if ids[narrow] >= pos {
				t.Errorf("expected %q (narrow) before e2e (broad); positions %v", narrow, ids)
			}
		}
	}
}

func TestInferCategories_FiltersNoiseAndUbiquitous(t *testing.T) {
	jobs := []string{
		"periodic-proj-e2e-main",
		"periodic-proj-e2e-release-1-23",
		"periodic-proj-e2e-release-1-24",
	}
	rules := InferCategories(jobs)
	for _, r := range rules {
		switch r.ID {
		case "periodic", "main", "release", "proj", "1", "23", "24":
			t.Errorf("noise/ubiquitous token %q became a category", r.ID)
		}
	}
	// "proj" and "e2e" appear in ALL jobs -> not distinguishers -> excluded.
}

func TestInferCategories_EdgeCases(t *testing.T) {
	if r := InferCategories(nil); r != nil {
		t.Errorf("nil input: want nil, got %v", r)
	}
	if r := InferCategories([]string{"only-one-job"}); r != nil {
		t.Errorf("single job: want nil, got %v", r)
	}
	// Two identical-shape jobs differing only by version: no distinguishing
	// token -> flat grid (nil).
	if r := InferCategories([]string{"job-main", "job-release-1-23"}); len(r) != 0 {
		t.Errorf("no distinguisher: want nil, got %v", r)
	}
}

func TestInferCategories_RespectsCap(t *testing.T) {
	var jobs []string
	for i := 0; i < 30; i++ {
		jobs = append(jobs, "periodic-proj-flavor"+string(rune('a'+i))+"-main")
	}
	rules := InferCategories(jobs)
	if len(rules) > maxCategories {
		t.Errorf("got %d categories, want <= %d", len(rules), maxCategories)
	}
}

func TestInferCategories_NeverEmitsReservedOther(t *testing.T) {
	// "other" is the engine's reserved fallback id; a token "other" must never
	// become a category (project.Validate would reject it).
	jobs := []string{
		"periodic-proj-other-main", "periodic-proj-other-release-1-23",
		"periodic-proj-foo-main",
	}
	for _, r := range InferCategories(jobs) {
		if r.ID == "other" {
			t.Error("emitted reserved category id \"other\"")
		}
	}
}

func TestInferCategories_SubstringCoverage(t *testing.T) {
	// "capi" contains "api"; coverage must use the engine's substring semantics
	// so the proposed rules validate and classify as they will at runtime.
	jobs := []string{
		"periodic-capi-e2e-main", "periodic-capi-e2e-release-1-23",
	}
	// Both jobs share "capi" and "e2e" as exact tokens but those appear in ALL
	// jobs, so there's no distinguisher -> flat grid. Just assert it stays valid
	// (no panic, no reserved/whitespace ids) and produces a loadable set.
	rules := InferCategories(jobs)
	for _, r := range rules {
		if strings.TrimSpace(r.ID) != r.ID || r.ID == "" {
			t.Errorf("bad id %q", r.ID)
		}
	}
}

func TestLabelFor(t *testing.T) {
	cases := map[string]string{
		"aks":          "AKS",
		"e2e":          "E2E",
		"ci":           "CI",
		"dual-stack":   "Dual Stack",
		"machine-pool": "Machine Pool",
		"conformance":  "Conformance",
	}
	for in, want := range cases {
		if got := labelFor(in); got != want {
			t.Errorf("labelFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------- scaffold rendering + validation ----------

func testOpts() Options {
	return Options{
		TestGrid:      "my-dashboard",
		DashboardRepo: "my-org/my-proj-prow-ai-dashboard",
		SourceRepo:    "upstream/my-proj",
		EngineRef:     "main",
	}
}

func TestRenderProjectYAML_ValidatesForTestGrid(t *testing.T) {
	opts := testOpts()
	data := buildScaffoldData(opts, InferCategories([]string{
		"periodic-myproj-e2e-main", "periodic-myproj-conformance-main",
	}))
	yamlText, err := renderProjectYAML(data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := validateGeneratedYAML(yamlText); err != nil {
		t.Fatalf("generated yaml failed validation: %v\n---\n%s", err, yamlText)
	}
	// Spot-check derived fields.
	for _, want := range []string{
		`dashboard: "my-dashboard"`,
		`provider: gcs`,
		`bucket: "kubernetes-ci-logs"`,
		`base_path: "/my-proj-prow-ai-dashboard"`,
		`site_url: "https://my-org.github.io/my-proj-prow-ai-dashboard"`,
		`owner: "upstream"`,
		`name: "my-proj"`,
	} {
		if !strings.Contains(yamlText, want) {
			t.Errorf("project.yaml missing %q\n---\n%s", want, yamlText)
		}
	}
	// id derived from the dashboard repo with the dashboard suffix stripped.
	if !strings.Contains(yamlText, "id: my-proj") {
		t.Errorf("expected id my-proj derived from repo name\n%s", yamlText)
	}
}

func TestRenderProjectYAML_ValidatesForBucketGCSWeb(t *testing.T) {
	opts := Options{
		Bucket:        "istio-prow",
		GCSWebBase:    "https://gcsweb.istio.io/s3",
		DashboardRepo: "me/istio-dash",
		SourceRepo:    "istio/istio",
		EngineRef:     "main",
	}
	data := buildScaffoldData(opts, nil)
	yamlText, err := renderProjectYAML(data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := validateGeneratedYAML(yamlText); err != nil {
		t.Fatalf("gcsweb yaml failed validation: %v\n---\n%s", err, yamlText)
	}
	for _, want := range []string{
		`source: bucket`,
		`provider: gcsweb`,
		`bucket: "istio-prow"`,
		`base: "https://gcsweb.istio.io/s3"`,
	} {
		if !strings.Contains(yamlText, want) {
			t.Errorf("bucket yaml missing %q\n---\n%s", want, yamlText)
		}
	}
	// No categories block when none inferred.
	if strings.Contains(yamlText, "categories:") {
		t.Errorf("did not expect a categories block\n%s", yamlText)
	}
}

func TestRenderProjectYAML_NoBlankLineRuns(t *testing.T) {
	data := buildScaffoldData(testOpts(), nil)
	yamlText, err := renderProjectYAML(data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(yamlText, "\n\n\n") {
		t.Errorf("found a run of blank lines:\n%s", yamlText)
	}
}

// ---------- options validation ----------

func TestValidateOptions(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Options)
		wantErr string
	}{
		{"both selectors", func(o *Options) { o.Bucket = "b" }, "exactly one"},
		{"no selector", func(o *Options) { o.TestGrid = "" }, "exactly one"},
		{"missing dashboard repo", func(o *Options) { o.DashboardRepo = "" }, "dashboard-repo"},
		{"missing source repo", func(o *Options) { o.SourceRepo = "" }, "source-repo"},
		{"bad dashboard repo", func(o *Options) { o.DashboardRepo = "noslash" }, "owner/name"},
		{"trailing slash repo", func(o *Options) { o.DashboardRepo = "owner/" }, "owner/name"},
		{"three-part repo", func(o *Options) { o.SourceRepo = "a/b/c" }, "owner/name"},
		{"gcsweb without bucket", func(o *Options) { o.GCSWebBase = "https://x" }, "gcsweb-base"},
		{"ai token without endpoint or model", func(o *Options) { o.AIToken = "t" }, "AI_ENDPOINT and AI_MODEL"},
		{"ai token without model", func(o *Options) { o.AIToken = "t"; o.AIEndpoint = "https://x" }, "AI_ENDPOINT and AI_MODEL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOpts()
			tc.mutate(&opts)
			err := validateOptions(&opts)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateOptions_DefaultsOutDir(t *testing.T) {
	opts := testOpts()
	if err := validateOptions(&opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.OutDir != "my-proj-prow-ai-dashboard" {
		t.Errorf("OutDir = %q, want the dashboard repo name", opts.OutDir)
	}
	if opts.EngineRef != "main" {
		t.Errorf("EngineRef = %q, want main", opts.EngineRef)
	}
}

// TestValidateOptions_AIProviderExplicit checks that AI drafting requires both
// the endpoint and model (no assumed default), but -no-prompt or a full provider
// config passes.
func TestValidateOptions_AIProviderExplicit(t *testing.T) {
	t.Run("full provider ok", func(t *testing.T) {
		opts := testOpts()
		opts.AIToken, opts.AIEndpoint, opts.AIModel = "t", "https://x/chat/completions", "m"
		if err := validateOptions(&opts); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("no-prompt skips the requirement", func(t *testing.T) {
		opts := testOpts()
		opts.AIToken, opts.NoPrompt = "t", true
		if err := validateOptions(&opts); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// confirms the engine's own LoadDir accepts it: the prompt stub is non-empty
// (LoadDir rejects an empty prompt) and the config validates.
func TestScaffold_LoadsViaLoadDir(t *testing.T) {
	data := buildScaffoldData(testOpts(), InferCategories([]string{
		"periodic-myproj-e2e-main", "periodic-myproj-e2e-release-1-23",
		"periodic-myproj-conformance-main", "periodic-myproj-conformance-release-1-23",
	}))

	projectYAML, err := renderProjectYAML(data)
	if err != nil {
		t.Fatalf("render project.yaml: %v", err)
	}
	prompt, err := render(systemPromptTmpl, data)
	if err != nil {
		t.Fatalf("render prompt: %v", err)
	}
	dir := t.TempDir()
	files := map[string]string{
		"project.yaml":      projectYAML,
		"prompts/system.md": prompt,
	}
	if err := writeFiles(dir, files); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, gotPrompt, err := project.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir rejected the scaffold: %v", err)
	}
	if cfg.ID == "" || cfg.Name == "" {
		t.Errorf("loaded config missing id/name: %+v", cfg)
	}
	if strings.TrimSpace(gotPrompt) == "" {
		t.Error("prompt stub must be non-empty (LoadDir requires it)")
	}

	// Writing again into the same dir must refuse rather than clobber.
	if err := writeFiles(dir, files); err == nil {
		t.Error("expected writeFiles to refuse overwriting existing files")
	}
}
