package jobconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// fakeTestInfra hosts a stub of both the GitHub Contents API listing and
// the raw.githubusercontent.com download endpoint. files maps a
// repo-relative path (e.g. "config/jobs/foo/bar.yaml") to the raw YAML
// the fetcher would download.
type fakeTestInfra struct {
	files map[string]string
}

func (f *fakeTestInfra) start(t *testing.T) (rawURL, apiURL string, stop func()) {
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
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		dir := strings.TrimPrefix(r.URL.Path, "/api/")
		dir = strings.TrimSuffix(dir, "/")
		type entry struct {
			Name string `json:"name"`
			Type string `json:"type"`
		}
		var out []entry
		for path := range f.files {
			parent := path[:strings.LastIndex(path, "/")]
			if parent != dir {
				continue
			}
			out = append(out, entry{Name: path[len(parent)+1:], Type: "file"})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	srv := httptest.NewServer(mux)
	return srv.URL + "/raw/", srv.URL + "/api/", srv.Close
}

func setURLs(t *testing.T, raw, api string) {
	t.Helper()
	origRaw, origAPI := rawBaseURL, apiBaseURL
	rawBaseURL, apiBaseURL = raw, api
	t.Cleanup(func() {
		rawBaseURL, apiBaseURL = origRaw, origAPI
	})
}

func TestFetchJobConfigs_MultiplePathsReturnsUnion(t *testing.T) {
	const dashboard = "d"
	job := func(name string) string {
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
	tf := &fakeTestInfra{files: map[string]string{
		"config/jobs/k/sig-a/x.yaml": job("a-x"),
		"config/jobs/k/sig-b/x.yaml": job("b-x"),
		"config/jobs/k/sig-b/y.yaml": job("b-y"),
	}}
	raw, api, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, api)

	cfg := &project.Config{
		Source: project.Source{
			TestInfraPaths: []string{
				"config/jobs/k/sig-a",
				"config/jobs/k/sig-b",
			},
		},
		TestGrid: project.TestGrid{Dashboard: dashboard},
	}

	jobs, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err != nil {
		t.Fatalf("FetchJobConfigs: %v", err)
	}
	if got := len(jobs); got != 3 {
		t.Fatalf("expected 3 jobs across all paths, got %d", got)
	}

	// Same-basename files in different paths must each carry their
	// full repo-relative ConfigFile so the "view job config" link works
	// and so the parser doesn't silently drop one.
	seenConfigFiles := map[string]string{}
	for _, j := range jobs {
		seenConfigFiles[j.Name] = j.ConfigFile
	}
	if seenConfigFiles["a-x"] != "config/jobs/k/sig-a/x.yaml" {
		t.Errorf("a-x ConfigFile = %q", seenConfigFiles["a-x"])
	}
	if seenConfigFiles["b-x"] != "config/jobs/k/sig-b/x.yaml" {
		t.Errorf("b-x ConfigFile = %q", seenConfigFiles["b-x"])
	}
	if seenConfigFiles["b-y"] != "config/jobs/k/sig-b/y.yaml" {
		t.Errorf("b-y ConfigFile = %q", seenConfigFiles["b-y"])
	}
}

func TestFetchJobConfigs_ZeroMatchErrors(t *testing.T) {
	tf := &fakeTestInfra{files: map[string]string{
		"config/jobs/k/sig-a/x.yaml": `periodics:
- name: foo
  minimum_interval: 1h
  annotations:
    testgrid-dashboards: other-dashboard
    testgrid-tab-name: foo
  extra_refs:
  - org: o
    repo: r
    base_ref: main
`,
	}}
	raw, api, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, api)

	cfg := &project.Config{
		Source:   project.Source{TestInfraPaths: []string{"config/jobs/k/sig-a"}},
		TestGrid: project.TestGrid{Dashboard: "wanted"},
	}

	_, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err == nil {
		t.Fatal("expected zero-match error, got nil")
	}
	for _, want := range []string{"no jobs labeled with dashboard", `"wanted"`, "config/jobs/k/sig-a"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestFetchJobConfigs_EmptyFilePrefixAcceptsAll(t *testing.T) {
	const dashboard = "d"
	job := func(name string) string {
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
	tf := &fakeTestInfra{files: map[string]string{
		"config/jobs/k/sig-a/alpha.yaml":   job("alpha"),
		"config/jobs/k/sig-a/beta.yaml":    job("beta"),
		"config/jobs/k/sig-a/presets.yaml": job("ignored"),
	}}
	raw, api, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, api)

	cfg := &project.Config{
		Source: project.Source{
			TestInfraPaths: []string{"config/jobs/k/sig-a"},
			// FilePrefix omitted: every *.yaml (minus presets) should be parsed.
		},
		TestGrid: project.TestGrid{Dashboard: dashboard},
	}

	jobs, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err != nil {
		t.Fatalf("FetchJobConfigs: %v", err)
	}
	if got := len(jobs); got != 2 {
		t.Fatalf("expected 2 jobs (presets.yaml excluded), got %d", got)
	}
}
