package onboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakePRGitHub is an in-memory GitHub git-data + pulls API for tests.
type fakePRGitHub struct {
	*httptest.Server
	defaultBranch string // empty => repo has no default branch (uninitialized)
	createdTree   map[string]string
	createdBranch string
	prHead        string
	prBase        string
	prTitle       string
	prBody        string
}

func newFakePRGitHub(t *testing.T, defaultBranch string) *fakePRGitHub {
	t.Helper()
	f := &fakePRGitHub{defaultBranch: defaultBranch}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Close)
	return f
}

func (f *fakePRGitHub) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p := r.URL.Path
	switch {
	// GET repo
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/repos/o/r"):
		writePRJSON(w, 200, map[string]any{"default_branch": f.defaultBranch})
	case r.Method == http.MethodGet && strings.Contains(p, "/git/ref/heads/"):
		if f.defaultBranch == "" {
			http.Error(w, "not found", 404)
			return
		}
		writePRJSON(w, 200, map[string]any{"object": map[string]any{"sha": "basesha"}})
	case r.Method == http.MethodGet && strings.Contains(p, "/git/commits/"):
		writePRJSON(w, 200, map[string]any{"tree": map[string]any{"sha": "basetree"}})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/git/trees"):
		var in struct {
			BaseTree string `json:"base_tree"`
			Tree     []struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			} `json:"tree"`
		}
		_ = json.Unmarshal(body, &in)
		f.createdTree = map[string]string{}
		for _, e := range in.Tree {
			f.createdTree[e.Path] = e.Content
		}
		writePRJSON(w, 201, map[string]any{"sha": "newtree"})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/git/commits"):
		writePRJSON(w, 201, map[string]any{"sha": "newcommit"})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/git/refs"):
		var in struct {
			Ref string `json:"ref"`
		}
		_ = json.Unmarshal(body, &in)
		f.createdBranch = in.Ref
		writePRJSON(w, 201, map[string]any{"ref": in.Ref})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/pulls"):
		var in struct {
			Title, Body, Head, Base string
		}
		_ = json.Unmarshal(body, &in)
		f.prHead, f.prBase, f.prTitle, f.prBody = in.Head, in.Base, in.Title, in.Body
		writePRJSON(w, 201, map[string]any{"html_url": "https://github.com/o/r/pull/7"})
	default:
		http.Error(w, "unexpected "+r.Method+" "+p, 500)
	}
}

func writePRJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// patchedAPIBase rewrites the hardcoded api.github.com base to the test server
// for the duration of a test. The writeClient builds URLs from a fixed host, so
// we point it at the fake by overriding the base via a small shim.
func newTestWriteClient(f *fakePRGitHub) *writeClient {
	return &writeClient{client: f.Client(), token: "t", owner: "o", repo: "r", base: f.Server.URL}
}

func TestOpenScaffoldPR_HappyPath(t *testing.T) {
	f := newFakePRGitHub(t, "main")
	files := map[string]string{
		"project.yaml":      "id: x",
		"prompts/system.md": "stub",
	}
	url, err := openScaffoldPRWith(context.Background(), newTestWriteClient(f), files, "title", "body")
	if err != nil {
		t.Fatalf("openScaffoldPR: %v", err)
	}
	if url != "https://github.com/o/r/pull/7" {
		t.Errorf("url = %q", url)
	}
	if f.createdTree["project.yaml"] != "id: x" || f.createdTree["prompts/system.md"] != "stub" {
		t.Errorf("tree missing files: %v", f.createdTree)
	}
	if !strings.HasPrefix(f.createdBranch, "refs/heads/onboard/scaffold-") {
		t.Errorf("branch = %q, want onboard/scaffold-*", f.createdBranch)
	}
	if f.prBase != "main" {
		t.Errorf("PR base = %q, want main", f.prBase)
	}
	if f.prHead == "" || f.prTitle != "title" {
		t.Errorf("PR head/title wrong: head=%q title=%q", f.prHead, f.prTitle)
	}
}

func TestOpenScaffoldPR_EmptyRepoErrors(t *testing.T) {
	f := newFakePRGitHub(t, "") // no default branch
	_, err := openScaffoldPRWith(context.Background(), newTestWriteClient(f), map[string]string{"a": "b"}, "t", "b")
	if err == nil || !strings.Contains(err.Error(), "initialize") {
		t.Errorf("expected an initialize-the-repo error, got %v", err)
	}
}

func TestOpenScaffoldPR_NoToken(t *testing.T) {
	_, err := openScaffoldPR(context.Background(), http.DefaultClient, "", "o", "r", nil, "t", "b")
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("expected a token error, got %v", err)
	}
}
