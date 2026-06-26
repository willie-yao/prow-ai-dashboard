package onboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// openScaffoldPR commits the scaffold files to a new branch in the dashboard
// repo and opens a pull request against its default branch. It uses the Git
// Data API so all files land in one clean commit. Returns the PR's html URL.
func openScaffoldPR(ctx context.Context, client *http.Client, token, owner, repo string, files map[string]string, title, body string) (string, error) {
	if token == "" {
		return "", fmt.Errorf("--open-pr needs a GitHub token with write access to %s/%s (set GITHUB_TOKEN)", owner, repo)
	}
	gh := &writeClient{client: client, token: token, owner: owner, repo: repo, base: githubAPIBase}
	return openScaffoldPRWith(ctx, gh, files, title, body)
}

// openScaffoldPRWith runs the PR flow against a prepared writeClient (so tests
// can point it at a stub server).
func openScaffoldPRWith(ctx context.Context, gh *writeClient, files map[string]string, title, body string) (string, error) {
	branch, headSHA, baseTree, err := gh.baseRef(ctx)
	if err != nil {
		return "", err
	}
	tree, err := gh.createTree(ctx, baseTree, files)
	if err != nil {
		return "", err
	}
	commit, err := gh.createCommit(ctx, title, tree, headSHA)
	if err != nil {
		return "", err
	}
	newBranch := fmt.Sprintf("onboard/scaffold-%d-%s", time.Now().Unix(), randomSuffix())
	if err := gh.createRef(ctx, "refs/heads/"+newBranch, commit); err != nil {
		return "", err
	}
	return gh.createPR(ctx, title, body, newBranch, branch)
}

// randomSuffix returns a short random hex string so concurrent runs in the same
// second don't collide on the scaffold branch name.
func randomSuffix() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// githubAPIBase is the GitHub REST API root, overridable per writeClient for
// tests.
const githubAPIBase = "https://api.github.com"

// writeClient holds the auth + target repo for the write operations.
type writeClient struct {
	client *http.Client
	token  string
	owner  string
	repo   string
	base   string
}

func (w *writeClient) url(suffix string) string {
	if suffix == "" {
		return fmt.Sprintf("%s/repos/%s/%s", w.base, w.owner, w.repo)
	}
	return fmt.Sprintf("%s/repos/%s/%s/%s", w.base, w.owner, w.repo, suffix)
}

// baseRef returns the repo's default branch, its head commit SHA, and the SHA
// of the tree that commit points at.
func (w *writeClient) baseRef(ctx context.Context) (branch, headSHA, treeSHA string, err error) {
	var repoInfo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err = w.get(ctx, w.url(""), &repoInfo); err != nil {
		return "", "", "", err
	}
	if repoInfo.DefaultBranch == "" {
		return "", "", "", fmt.Errorf("repo %s/%s has no default branch; initialize it (e.g. add a README) before opening a scaffold PR", w.owner, w.repo)
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err = w.get(ctx, w.url("git/ref/heads/"+repoInfo.DefaultBranch), &ref); err != nil {
		return "", "", "", fmt.Errorf("reading %s head (is the repo empty? initialize it first): %w", repoInfo.DefaultBranch, err)
	}
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err = w.get(ctx, w.url("git/commits/"+ref.Object.SHA), &commit); err != nil {
		return "", "", "", err
	}
	return repoInfo.DefaultBranch, ref.Object.SHA, commit.Tree.SHA, nil
}

// createTree builds a new tree from baseTree with the scaffold files added.
func (w *writeClient) createTree(ctx context.Context, baseTree string, files map[string]string) (string, error) {
	type entry struct {
		Path    string `json:"path"`
		Mode    string `json:"mode"`
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	// Deterministic order for stable requests/tests.
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	entries := make([]entry, 0, len(files))
	for _, p := range paths {
		entries = append(entries, entry{Path: p, Mode: "100644", Type: "blob", Content: files[p]})
	}
	var out struct {
		SHA string `json:"sha"`
	}
	err := w.post(ctx, w.url("git/trees"), map[string]any{"base_tree": baseTree, "tree": entries}, &out)
	return out.SHA, err
}

func (w *writeClient) createCommit(ctx context.Context, message, tree, parent string) (string, error) {
	var out struct {
		SHA string `json:"sha"`
	}
	err := w.post(ctx, w.url("git/commits"), map[string]any{
		"message": message,
		"tree":    tree,
		"parents": []string{parent},
	}, &out)
	return out.SHA, err
}

func (w *writeClient) createRef(ctx context.Context, ref, sha string) error {
	return w.post(ctx, w.url("git/refs"), map[string]any{"ref": ref, "sha": sha}, nil)
}

func (w *writeClient) createPR(ctx context.Context, title, body, head, base string) (string, error) {
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	err := w.post(ctx, w.url("pulls"), map[string]any{
		"title": title, "body": body, "head": head, "base": base,
	}, &out)
	return out.HTMLURL, err
}

func (w *writeClient) get(ctx context.Context, url string, out any) error {
	return w.do(ctx, http.MethodGet, url, nil, out, http.StatusOK)
}

func (w *writeClient) post(ctx context.Context, url string, body, out any) error {
	return w.do(ctx, http.MethodPost, url, body, out, http.StatusCreated, http.StatusOK)
}

func (w *writeClient) do(ctx context.Context, method, url string, body, out any, okStatuses ...int) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "prow-ai-dashboard")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	ok := false
	for _, s := range okStatuses {
		if resp.StatusCode == s {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, truncate(string(rb), 300))
	}
	if out != nil {
		return json.Unmarshal(rb, out)
	}
	return nil
}
