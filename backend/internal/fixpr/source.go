package fixpr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// sourceReader fetches a file's current content from a GitHub repo via the
// contents API at a given ref. An interface so the generator is unit-testable.
type sourceReader interface {
	// FileContent returns the file at path on owner/repo at ref (a branch, tag,
	// or commit SHA; empty means the default branch). found is false (no error)
	// when the file does not exist.
	FileContent(ctx context.Context, owner, repo, ref, path string) (content string, found bool, err error)
}

// httpSource reads files from github.com via the REST contents API.
type httpSource struct {
	client *http.Client
	token  string
	base   string
}

func newHTTPSource(token string) *httpSource {
	return &httpSource{
		client: &http.Client{Timeout: 30 * time.Second},
		token:  token,
		base:   "https://api.github.com",
	}
}

func (s *httpSource) FileContent(ctx context.Context, owner, repo, ref, path string) (string, bool, error) {
	// Escape each path segment but preserve the slashes.
	escaped := strings.Join(mapStrings(strings.Split(path, "/"), url.PathEscape), "/")
	u := fmt.Sprintf("%s/repos/%s/%s/contents/%s", s.base, owner, repo, escaped)
	if ref != "" {
		u += "?ref=" + url.QueryEscape(ref)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", false, err
	}
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "prow-ai-dashboard")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("reading %s: %s: %s", path, resp.Status, truncate(string(rb), 200))
	}
	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Type     string `json:"type"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", false, fmt.Errorf("decoding %s: %w", path, err)
	}
	if out.Type != "file" {
		return "", false, fmt.Errorf("%s is not a file (type %q)", path, out.Type)
	}
	if out.Encoding != "base64" {
		return "", false, fmt.Errorf("%s has unexpected encoding %q", path, out.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil {
		return "", false, fmt.Errorf("decoding %s content: %w", path, err)
	}
	return string(decoded), true, nil
}

func mapStrings(in []string, f func(string) string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = f(s)
	}
	return out
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
