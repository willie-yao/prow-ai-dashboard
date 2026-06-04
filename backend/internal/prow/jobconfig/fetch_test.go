package jobconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// fakeTestInfra hosts a stub of both the GitHub Code Search endpoint and
// the raw.githubusercontent.com download endpoint. files maps a
// repo-relative path (e.g. "config/jobs/foo/bar.yaml") to the raw YAML
// the fetcher would download. Code search returns every path whose body
// contains the dashboard string from the query.
type fakeTestInfra struct {
	files             map[string]string
	forceIncomplete   bool // set incomplete_results=true on every page
	pageSize          int  // results per page (defaults to 100)
	forcedStatus      int  // when nonzero, returned instead of 200 on /search
	forcedSearchBody  string
	extraNonMatchPath string // a yaml path that the search returns despite not containing the dashboard string (for false-positive coverage)
}

func (f *fakeTestInfra) start(t *testing.T) (rawURL, searchURL string, stop func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/raw/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/raw/")
		body, ok := f.files[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/search/code", func(w http.ResponseWriter, r *http.Request) {
		if f.forcedStatus != 0 {
			w.WriteHeader(f.forcedStatus)
			_, _ = w.Write([]byte(f.forcedSearchBody))
			return
		}
		q := r.URL.Query().Get("q")
		// Quote-delimited dashboard string is the first token. Extract
		// what's between the quotes.
		dash := ""
		if i := strings.Index(q, `"`); i >= 0 {
			if j := strings.Index(q[i+1:], `"`); j >= 0 {
				dash = q[i+1 : i+1+j]
			}
		}
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			_, _ = fmt.Sscanf(p, "%d", &page)
		}
		size := f.pageSize
		if size == 0 {
			size = 100
		}

		var matching []string
		for path, body := range f.files {
			if dash != "" && !strings.Contains(body, dash) {
				continue
			}
			matching = append(matching, path)
		}
		if f.extraNonMatchPath != "" {
			matching = append(matching, f.extraNonMatchPath)
		}
		sort.Strings(matching)

		total := len(matching)
		start := (page - 1) * size
		end := start + size
		if start > total {
			start = total
		}
		if end > total {
			end = total
		}
		items := make([]map[string]string, 0, end-start)
		for _, p := range matching[start:end] {
			items = append(items, map[string]string{"path": p})
		}
		out := map[string]any{
			"total_count":        total,
			"incomplete_results": f.forceIncomplete,
			"items":              items,
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	srv := httptest.NewServer(mux)
	return srv.URL + "/raw/", srv.URL + "/search/code", srv.Close
}

func setURLs(t *testing.T, raw, search string) {
	t.Helper()
	origRaw, origSearch := rawBaseURL, searchBaseURL
	rawBaseURL, searchBaseURL = raw, search
	t.Cleanup(func() {
		rawBaseURL, searchBaseURL = origRaw, origSearch
	})
}

func setToken(t *testing.T, token string) {
	t.Helper()
	prev, hadPrev := os.LookupEnv("GITHUB_TOKEN")
	if token == "" {
		_ = os.Unsetenv("GITHUB_TOKEN")
	} else {
		t.Setenv("GITHUB_TOKEN", token)
	}
	t.Cleanup(func() {
		if hadPrev {
			_ = os.Setenv("GITHUB_TOKEN", prev)
		} else {
			_ = os.Unsetenv("GITHUB_TOKEN")
		}
	})
}

func periodicJob(name, dashboard string) string {
	return fmt.Sprintf(`periodics:
- name: %s
  minimum_interval: 24h
  annotations:
    testgrid-dashboards: %s
    testgrid-tab-name: %s
  extra_refs:
  - org: o
    repo: r
    base_ref: main
`, name, dashboard, name)
}

func TestFetchJobConfigs_DiscoversAcrossDirectoriesAndNames(t *testing.T) {
	const dashboard = "sig-cluster-lifecycle-cluster-api-provider-azure"
	// Mirrors the real-world CAPZ case: matching files live in two
	// different directories AND have different filename prefixes.
	tf := &fakeTestInfra{files: map[string]string{
		"config/jobs/kubernetes-sigs/cluster-api-provider-azure/cluster-api-provider-azure-periodics-main.yaml": periodicJob("periodic-cluster-api-provider-azure-e2e", dashboard),
		"config/jobs/kubernetes/sig-scalability/sig-scalability-periodic-azure.yaml":                            periodicJob("ci-kubernetes-e2e-azure-scalability", dashboard),
	}}
	raw, search, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, search)
	setToken(t, "fake-token")

	cfg := &project.Config{
		TestGrid: project.TestGrid{Dashboard: dashboard},
	}

	jobs, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err != nil {
		t.Fatalf("FetchJobConfigs: %v", err)
	}
	if got := len(jobs); got != 2 {
		t.Fatalf("expected 2 jobs, got %d (%+v)", got, jobs)
	}

	wantConfigFiles := map[string]string{
		"periodic-cluster-api-provider-azure-e2e": "config/jobs/kubernetes-sigs/cluster-api-provider-azure/cluster-api-provider-azure-periodics-main.yaml",
		"ci-kubernetes-e2e-azure-scalability":     "config/jobs/kubernetes/sig-scalability/sig-scalability-periodic-azure.yaml",
	}
	for _, j := range jobs {
		want, ok := wantConfigFiles[j.Name]
		if !ok {
			t.Errorf("unexpected job %q", j.Name)
			continue
		}
		if j.ConfigFile != want {
			t.Errorf("%s ConfigFile = %q, want %q", j.Name, j.ConfigFile, want)
		}
	}
}

func TestFetchJobConfigs_FalsePositiveFilteredByAnnotation(t *testing.T) {
	const dashboard = "wanted-dashboard"
	tf := &fakeTestInfra{
		files: map[string]string{
			"config/jobs/k/sig-a/match.yaml": periodicJob("foo", dashboard),
			// Search returns this file (it contains the dashboard string
			// in a comment) but its job is annotated for a different
			// dashboard. matchesDashboard must drop it.
			"config/jobs/k/sig-b/false-positive.yaml": `# mentions wanted-dashboard in a comment
periodics:
- name: bar
  minimum_interval: 1h
  annotations:
    testgrid-dashboards: some-other-dashboard
    testgrid-tab-name: bar
  extra_refs:
  - org: o
    repo: r
    base_ref: main
`,
		},
	}
	raw, search, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, search)
	setToken(t, "fake-token")

	cfg := &project.Config{TestGrid: project.TestGrid{Dashboard: dashboard}}
	jobs, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err != nil {
		t.Fatalf("FetchJobConfigs: %v", err)
	}
	if got := len(jobs); got != 1 || jobs[0].Name != "foo" {
		t.Fatalf("expected only the annotated job, got %+v", jobs)
	}
}

func TestFetchJobConfigs_ZeroMatchErrors(t *testing.T) {
	tf := &fakeTestInfra{files: map[string]string{}}
	raw, search, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, search)
	setToken(t, "fake-token")

	cfg := &project.Config{TestGrid: project.TestGrid{Dashboard: "wanted"}}
	_, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err == nil {
		t.Fatal("expected zero-match error, got nil")
	}
	for _, want := range []string{"no YAML files", `"wanted"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestFetchJobConfigs_RequiresGitHubToken(t *testing.T) {
	tf := &fakeTestInfra{files: map[string]string{}}
	raw, search, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, search)
	setToken(t, "")

	cfg := &project.Config{TestGrid: project.TestGrid{Dashboard: "d"}}
	_, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err == nil {
		t.Fatal("expected missing-token error, got nil")
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("error should mention GITHUB_TOKEN; got: %v", err)
	}
}

func TestFetchJobConfigs_IncompleteResultsIsError(t *testing.T) {
	tf := &fakeTestInfra{
		files: map[string]string{
			"config/jobs/k/a.yaml": periodicJob("a", "d"),
		},
		forceIncomplete: true,
	}
	raw, search, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, search)
	setToken(t, "fake-token")

	cfg := &project.Config{TestGrid: project.TestGrid{Dashboard: "d"}}
	_, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err == nil {
		t.Fatal("expected error for incomplete_results=true, got nil")
	}
	if !strings.Contains(err.Error(), "incomplete_results") {
		t.Errorf("error should mention incomplete_results; got: %v", err)
	}
}

func TestFetchJobConfigs_SearchHTTPErrorIsError(t *testing.T) {
	tf := &fakeTestInfra{
		files:            map[string]string{},
		forcedStatus:     http.StatusForbidden,
		forcedSearchBody: `{"message":"API rate limit exceeded"}`,
	}
	raw, search, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, search)
	setToken(t, "fake-token")

	cfg := &project.Config{TestGrid: project.TestGrid{Dashboard: "d"}}
	_, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err == nil {
		t.Fatal("expected HTTP 403 to surface, got nil")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("error should include status and response body; got: %v", err)
	}
}

func TestFetchJobConfigs_PaginatesAndDedupes(t *testing.T) {
	const dashboard = "d"
	files := map[string]string{}
	for i := 0; i < 5; i++ {
		files[fmt.Sprintf("config/jobs/k/a%d.yaml", i)] = periodicJob(fmt.Sprintf("a%d", i), dashboard)
	}
	tf := &fakeTestInfra{files: files, pageSize: 2}
	raw, search, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, search)
	setToken(t, "fake-token")

	cfg := &project.Config{TestGrid: project.TestGrid{Dashboard: dashboard}}
	jobs, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err != nil {
		t.Fatalf("FetchJobConfigs: %v", err)
	}
	if got := len(jobs); got != 5 {
		t.Fatalf("expected 5 jobs after pagination, got %d", got)
	}
}

func TestFetchJobConfigs_SearchQueryEncoded(t *testing.T) {
	// Dashboards with characters that would corrupt an unescaped query
	// (e.g. spaces, plus, ampersand) round-trip correctly through url.Values.
	const dashboard = `weird dashboard & name+test`
	tf := &fakeTestInfra{files: map[string]string{
		"config/jobs/k/a.yaml": periodicJob("a", dashboard),
	}}
	raw, search, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, search)
	setToken(t, "fake-token")

	cfg := &project.Config{TestGrid: project.TestGrid{Dashboard: dashboard}}
	jobs, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err != nil {
		t.Fatalf("FetchJobConfigs: %v", err)
	}
	if got := len(jobs); got != 1 {
		t.Fatalf("expected 1 job, got %d", got)
	}
}

func TestDerivePeriodicPrefix(t *testing.T) {
	cases := []struct {
		name string
		jobs []models.ProwJob
		want string
	}{
		{
			name: "majority shared prefix",
			jobs: []models.ProwJob{
				{Name: "periodic-cluster-api-provider-azure-e2e", JobType: models.JobTypePeriodic},
				{Name: "periodic-cluster-api-provider-azure-conformance", JobType: models.JobTypePeriodic},
				{Name: "periodic-cluster-api-provider-azure-upgrade", JobType: models.JobTypePeriodic},
				{Name: "ci-kubernetes-e2e-azure-scalability", JobType: models.JobTypePeriodic},
			},
			want: "periodic-cluster-api-provider-azure-",
		},
		{
			name: "no majority returns empty",
			jobs: []models.ProwJob{
				{Name: "periodic-foo-x", JobType: models.JobTypePeriodic},
				{Name: "periodic-bar-y", JobType: models.JobTypePeriodic},
			},
			want: "",
		},
		{
			name: "presubmits ignored",
			jobs: []models.ProwJob{
				{Name: "pull-foo-bar", JobType: models.JobTypePresubmit},
				{Name: "pull-foo-baz", JobType: models.JobTypePresubmit},
				{Name: "periodic-foo-bar", JobType: models.JobTypePeriodic},
				{Name: "periodic-foo-baz", JobType: models.JobTypePeriodic},
			},
			want: "periodic-foo-",
		},
		{
			name: "no periodics returns empty",
			jobs: []models.ProwJob{
				{Name: "pull-foo-bar", JobType: models.JobTypePresubmit},
			},
			want: "",
		},
		{
			name: "longest majority prefix wins",
			jobs: []models.ProwJob{
				{Name: "periodic-foo-bar-x", JobType: models.JobTypePeriodic},
				{Name: "periodic-foo-bar-y", JobType: models.JobTypePeriodic},
				{Name: "periodic-foo-bar-z", JobType: models.JobTypePeriodic},
			},
			want: "periodic-foo-bar-",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DerivePeriodicPrefix(tc.jobs); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
