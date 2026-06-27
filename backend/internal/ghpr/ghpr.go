// Package ghpr opens pull requests that add or update files in one commit using
// the GitHub Git Data API. It is shared by onboarding and skill suggestions.
package ghpr

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// apiBase is the GitHub REST API root, overridable per Client for tests.
const apiBase = "https://api.github.com"

// Client opens PRs against a GitHub repo with a single token identity.
type Client struct {
	httpClient *http.Client
	token      string
	base       string
}

// NewClient builds a Client. A nil httpClient defaults to a 30s client.
func NewClient(httpClient *http.Client, token string) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{httpClient: httpClient, token: token, base: apiBase}
}

// Request describes a PR to open: a file-set committed to a new branch off the
// repo's default branch, then a PR back to it.
type Request struct {
	Owner string
	Repo  string
	// Files maps repo-relative path to file content. Existing paths are
	// overwritten in the new commit; others are carried over from the base.
	Files map[string]string
	// BranchPrefix names the throwaway head branch. A unix-time and random suffix
	// are appended for uniqueness.
	BranchPrefix string
	Title        string
	Body         string
	// Draft opens the PR as a draft.
	Draft bool
	// Labels are applied to the PR after creation. The pulls API cannot set them
	// at creation time.
	Labels []string
	// AuthorName and AuthorEmail set the commit author. Empty uses the token's
	// identity. SignOff appends a DCO "Signed-off-by" trailer for that author.
	AuthorName  string
	AuthorEmail string
	SignOff     bool
}

// OpenPR commits the request's files to a new branch and opens a PR against the
// repo's default branch. Returns the PR's html URL.
func (c *Client) OpenPR(ctx context.Context, req Request) (string, error) {
	if c.token == "" {
		return "", fmt.Errorf("opening a PR needs a GitHub token with write access to %s/%s", req.Owner, req.Repo)
	}
	if len(req.Files) == 0 {
		return "", fmt.Errorf("no files to commit")
	}
	branch, headSHA, baseTree, err := c.baseRef(ctx, req.Owner, req.Repo)
	if err != nil {
		return "", err
	}
	tree, err := c.createTree(ctx, req.Owner, req.Repo, baseTree, req.Files)
	if err != nil {
		return "", err
	}
	commit, err := c.createCommit(ctx, req.Owner, req.Repo, commitMessage(req), tree, headSHA, req)
	if err != nil {
		return "", err
	}
	newBranch := fmt.Sprintf("%s-%d-%s", req.BranchPrefix, time.Now().Unix(), randomSuffix())
	if err := c.createRef(ctx, req.Owner, req.Repo, "refs/heads/"+newBranch, commit); err != nil {
		return "", err
	}
	number, htmlURL, err := c.createPR(ctx, req.Owner, req.Repo, req.Title, req.Body, newBranch, branch, req.Draft)
	if err != nil {
		return "", err
	}
	if len(req.Labels) > 0 {
		// A labeling failure should not discard an opened PR.
		if err := c.addLabels(ctx, req.Owner, req.Repo, number, req.Labels); err != nil {
			return htmlURL, fmt.Errorf("PR %s opened but labeling failed: %w", htmlURL, err)
		}
	}
	return htmlURL, nil
}

// commitMessage uses the PR title as the commit subject, adding a DCO sign-off
// trailer when requested and an author is set.
func commitMessage(req Request) string {
	msg := req.Title
	if req.SignOff && req.AuthorName != "" && req.AuthorEmail != "" {
		msg += fmt.Sprintf("\n\nSigned-off-by: %s <%s>", req.AuthorName, req.AuthorEmail)
	}
	return msg
}

// randomSuffix returns a short random hex string so concurrent runs in the same
// second don't collide on the branch name.
func randomSuffix() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

func (c *Client) url(owner, repo, suffix string) string {
	if suffix == "" {
		return fmt.Sprintf("%s/repos/%s/%s", c.base, owner, repo)
	}
	return fmt.Sprintf("%s/repos/%s/%s/%s", c.base, owner, repo, suffix)
}

// baseRef returns the repo's default branch, its head commit SHA, and the SHA
// of the tree that commit points at.
func (c *Client) baseRef(ctx context.Context, owner, repo string) (branch, headSHA, treeSHA string, err error) {
	var repoInfo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err = c.get(ctx, c.url(owner, repo, ""), &repoInfo); err != nil {
		return "", "", "", err
	}
	if repoInfo.DefaultBranch == "" {
		return "", "", "", fmt.Errorf("repo %s/%s has no default branch; initialize it (e.g. add a README) before opening a PR", owner, repo)
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err = c.get(ctx, c.url(owner, repo, "git/ref/heads/"+repoInfo.DefaultBranch), &ref); err != nil {
		return "", "", "", fmt.Errorf("reading %s head (is the repo empty? initialize it first): %w", repoInfo.DefaultBranch, err)
	}
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err = c.get(ctx, c.url(owner, repo, "git/commits/"+ref.Object.SHA), &commit); err != nil {
		return "", "", "", err
	}
	return repoInfo.DefaultBranch, ref.Object.SHA, commit.Tree.SHA, nil
}

// createTree builds a new tree from baseTree with the request's files added.
func (c *Client) createTree(ctx context.Context, owner, repo, baseTree string, files map[string]string) (string, error) {
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
	err := c.post(ctx, c.url(owner, repo, "git/trees"), map[string]any{"base_tree": baseTree, "tree": entries}, &out)
	return out.SHA, err
}

func (c *Client) createCommit(ctx context.Context, owner, repo, message, tree, parent string, req Request) (string, error) {
	payload := map[string]any{
		"message": message,
		"tree":    tree,
		"parents": []string{parent},
	}
	if req.AuthorName != "" && req.AuthorEmail != "" {
		payload["author"] = map[string]string{"name": req.AuthorName, "email": req.AuthorEmail}
	}
	var out struct {
		SHA string `json:"sha"`
	}
	err := c.post(ctx, c.url(owner, repo, "git/commits"), payload, &out)
	return out.SHA, err
}

func (c *Client) createRef(ctx context.Context, owner, repo, ref, sha string) error {
	return c.post(ctx, c.url(owner, repo, "git/refs"), map[string]any{"ref": ref, "sha": sha}, nil)
}

func (c *Client) createPR(ctx context.Context, owner, repo, title, body, head, base string, draft bool) (int, string, error) {
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	err := c.post(ctx, c.url(owner, repo, "pulls"), map[string]any{
		"title": title, "body": body, "head": head, "base": base, "draft": draft,
	}, &out)
	return out.Number, out.HTMLURL, err
}

// addLabels applies labels through the shared issues endpoint.
func (c *Client) addLabels(ctx context.Context, owner, repo string, number int, labels []string) error {
	return c.post(ctx, c.url(owner, repo, fmt.Sprintf("issues/%d/labels", number)),
		map[string]any{"labels": labels}, nil)
}

func (c *Client) get(ctx context.Context, url string, out any) error {
	return c.do(ctx, http.MethodGet, url, nil, out, http.StatusOK)
}

func (c *Client) post(ctx context.Context, url string, body, out any) error {
	return c.do(ctx, http.MethodPost, url, body, out, http.StatusCreated, http.StatusOK)
}

func (c *Client) do(ctx context.Context, method, url string, body, out any, okStatuses ...int) error {
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
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "prow-ai-dashboard")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
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

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// SearchOpenPR finds an open PR in owner/repo whose body contains confirmMarker.
// queryToken narrows the search. The full confirmMarker is checked to avoid
// token-level false positives when local state is lost.
func (c *Client) SearchOpenPR(ctx context.Context, owner, repo, queryToken, confirmMarker string) (number int, htmlURL string, found bool, err error) {
	q := fmt.Sprintf("repo:%s/%s is:pr is:open %s in:body", owner, repo, queryToken)
	searchURL := c.base + "/search/issues?per_page=5&q=" + url.QueryEscape(q)
	var out struct {
		Items []struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
			Body    string `json:"body"`
		} `json:"items"`
	}
	if err := c.get(ctx, searchURL, &out); err != nil {
		return 0, "", false, err
	}
	// GitHub search tokenizes bodies, so confirm the full marker before adoption.
	for _, it := range out.Items {
		if strings.Contains(it.Body, confirmMarker) {
			return it.Number, it.HTMLURL, true, nil
		}
	}
	return 0, "", false, nil
}
