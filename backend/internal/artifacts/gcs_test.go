package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestSafePath(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty", "", "", false},
		{"simple", "artifacts/foo", "artifacts/foo", false},
		{"trailing slash", "artifacts/", "artifacts", false},
		{"dot segments cleaned", "artifacts/./foo", "artifacts/foo", false},
		{"backslash rejected", "foo\\bar", "", true},
		{"control char rejected", "foo\x01bar", "", true},
		{"nul rejected", "foo\x00bar", "", true},
		{"absolute rejected", "/etc/passwd", "", true},
		{"url-looking rejected", "https://example.com/foo", "", true},
		{"escapes root", "../outside", "", true},
		{"deep dotdot rejected", "foo/../..", "", true},
		{"intermediate dotdot rejected", "foo/bar/../baz", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SafePath(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				if !errors.Is(err, ErrUnsafePath) {
					t.Fatalf("error must wrap ErrUnsafePath, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

// fakeGCS is a minimal httptest server that responds to GCS list + object GETs.
type fakeGCS struct {
	t     *testing.T
	files map[string][]byte // full object name (incl. logs/<job>/<build>/...) -> bytes

	// requests counts every request seen, keyed by method+URL path.
	requests map[string]int
}

func newFakeGCS(t *testing.T, files map[string][]byte) *fakeGCS {
	return &fakeGCS{t: t, files: files, requests: map[string]int{}}
}

func (f *fakeGCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	f.requests[key]++

	if strings.HasPrefix(r.URL.Path, "/storage/v1/b/") {
		f.serveList(w, r)
		return
	}
	f.serveObject(w, r)
}

func (f *fakeGCS) serveList(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	delim := r.URL.Query().Get("delimiter")
	type item struct {
		Name string `json:"name"`
		Size string `json:"size"`
	}
	resp := struct {
		Items    []item   `json:"items"`
		Prefixes []string `json:"prefixes"`
	}{}
	seenDirs := map[string]bool{}
	for name, body := range f.files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		if delim != "" && strings.Contains(rest, delim) {
			dir := rest[:strings.Index(rest, delim)+1]
			full := prefix + dir
			if !seenDirs[full] {
				seenDirs[full] = true
				resp.Prefixes = append(resp.Prefixes, full)
			}
			continue
		}
		resp.Items = append(resp.Items, item{Name: name, Size: strconv.Itoa(len(body))})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *fakeGCS) serveObject(w http.ResponseWriter, r *http.Request) {
	// Strip "/<bucket>/" prefix from path to get the object name.
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(parts) != 2 {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	name := parts[1]
	body, ok := f.files[name]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rng := r.Header.Get("Range")
	if rng == "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		w.Write(body)
		return
	}
	// Parse "bytes=start-end" or "bytes=-suffix".
	v := strings.TrimPrefix(rng, "bytes=")
	if strings.HasPrefix(v, "-") {
		// Suffix: last N bytes.
		n, _ := strconv.Atoi(strings.TrimPrefix(v, "-"))
		if n > len(body) {
			n = len(body)
		}
		start := len(body) - n
		end := len(body) - 1
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body[start:])
		return
	}
	dash := strings.Index(v, "-")
	start, _ := strconv.Atoi(v[:dash])
	end, _ := strconv.Atoi(v[dash+1:])
	if end >= len(body) {
		end = len(body) - 1
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
	w.WriteHeader(http.StatusPartialContent)
	w.Write(body[start : end+1])
}

// newBrowserWithFake spins up a fakeGCS and returns a GCSBrowser pointed at it.
func newBrowserWithFake(t *testing.T, files map[string][]byte) (*GCSBrowser, *fakeGCS, *httptest.Server) {
	t.Helper()
	fake := newFakeGCS(t, files)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	b := &GCSBrowser{
		bucket:    "test-bucket",
		display:   "job1/b1",
		client:    srv.Client(),
		prefix:    "logs/job1/b1/",
		objectURL: srv.URL + "/test-bucket/",
		listURL:   srv.URL + "/storage/v1/b/test-bucket/o",
		cache:     map[string][]byte{},
	}
	return b, fake, srv
}

func TestList(t *testing.T) {
	files := map[string][]byte{
		"logs/job1/b1/build-log.txt":             []byte("top-level"),
		"logs/job1/b1/started.json":              []byte("{}"),
		"logs/job1/b1/artifacts/junit.xml":       []byte("<x/>"),
		"logs/job1/b1/artifacts/clusters/a.yaml": []byte("a"),
	}
	b, _, _ := newBrowserWithFake(t, files)
	out, err := b.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Files) != 2 {
		t.Errorf("root files = %d, want 2 (build-log + started)", len(out.Files))
	}
	if len(out.Dirs) != 1 || out.Dirs[0] != "artifacts/" {
		t.Errorf("root dirs = %v, want [artifacts/]", out.Dirs)
	}

	sub, err := b.List(context.Background(), "artifacts/")
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.Files) != 1 || sub.Files[0].Name != "junit.xml" {
		t.Errorf("artifacts/ files = %v", sub.Files)
	}
	if len(sub.Dirs) != 1 || sub.Dirs[0] != "clusters/" {
		t.Errorf("artifacts/ dirs = %v", sub.Dirs)
	}
}

func TestList_UnsafePath(t *testing.T) {
	b, _, _ := newBrowserWithFake(t, nil)
	if _, err := b.List(context.Background(), "../outside"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("want ErrUnsafePath, got %v", err)
	}
}

func TestRead_RangeAndCache(t *testing.T) {
	files := map[string][]byte{
		"logs/job1/b1/artifacts/data.txt": []byte("0123456789ABCDEF"),
	}
	b, fake, _ := newBrowserWithFake(t, files)

	data, size, err := b.Read(context.Background(), "artifacts/data.txt", 4, 5)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "45678" {
		t.Errorf("read = %q, want 45678", data)
	}
	if size != 16 {
		t.Errorf("size = %d, want 16", size)
	}

	firstCalls := fake.requests["GET /test-bucket/logs/job1/b1/artifacts/data.txt"]

	// Read again with different offset; without cache this fires another HTTP call.
	_, _, _ = b.Read(context.Background(), "artifacts/data.txt", 0, 4)
	if fake.requests["GET /test-bucket/logs/job1/b1/artifacts/data.txt"] != firstCalls+1 {
		t.Errorf("want second HTTP call (no cache on Read)")
	}
}

func TestTail(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&sb, "line-%05d\n", i)
	}
	files := map[string][]byte{
		"logs/job1/b1/big.log": []byte(sb.String()),
	}
	b, _, _ := newBrowserWithFake(t, files)
	out, err := b.Tail(context.Background(), "big.log", 5, 32*1024)
	if err != nil {
		t.Fatal(err)
	}
	if out.LinesReturned != 5 {
		t.Errorf("LinesReturned = %d, want 5", out.LinesReturned)
	}
	content := string(out.Content)
	if !strings.Contains(content, "line-04999") {
		t.Errorf("tail missing last line; content=%q", content)
	}
	if !strings.Contains(content, "line-04995") {
		t.Errorf("tail should start ~line-04995; content=%q", content)
	}
}

func TestGrep_Streaming(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		if i%50 == 0 {
			fmt.Fprintf(&sb, "ERROR at line %d\n", i)
		} else {
			fmt.Fprintf(&sb, "info line %d\n", i)
		}
	}
	files := map[string][]byte{
		"logs/job1/b1/big.log": []byte(sb.String()),
	}
	b, _, _ := newBrowserWithFake(t, files)

	re := regexp.MustCompile(`ERROR`)
	res, err := b.Grep(context.Background(), "big.log", re, 1, 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalMatches != 4 {
		t.Errorf("TotalMatches = %d, want 4 (lines 0,50,100,150)", res.TotalMatches)
	}
	if len(res.Matches) != 4 {
		t.Errorf("Matches len = %d, want 4", len(res.Matches))
	}
	// Match at index 0 (line 1) should have NO before-context (it's the first line)
	// and one after-context line.
	first := res.Matches[0]
	if first.LineNo != 1 {
		t.Errorf("first match line = %d, want 1", first.LineNo)
	}
	// Each subsequent match should have 1 before + 1 marker + 1 after = 3 entries.
	if len(res.Matches[1].Context) != 3 {
		t.Errorf("match[1] context len = %d, want 3", len(res.Matches[1].Context))
	}
}

func TestGrep_Truncation(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintln(&sb, "MATCH")
	}
	files := map[string][]byte{
		"logs/job1/b1/big.log": []byte(sb.String()),
	}
	b, _, _ := newBrowserWithFake(t, files)
	res, err := b.Grep(context.Background(), "big.log", regexp.MustCompile(`MATCH`), 0, 5, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalMatches != 100 {
		t.Errorf("TotalMatches = %d, want 100", res.TotalMatches)
	}
	if len(res.Matches) != 5 {
		t.Errorf("Matches len = %d, want 5", len(res.Matches))
	}
	if !res.Truncated {
		t.Errorf("want Truncated = true")
	}
}

func TestListTree(t *testing.T) {
	files := map[string][]byte{
		"logs/job1/b1/build-log.txt":                              []byte("top"),
		"logs/job1/b1/started.json":                               []byte("{}"),
		"logs/job1/b1/artifacts/junit.xml":                        []byte("<x/>"),
		"logs/job1/b1/artifacts/clusters/c1/machines/m1/boot.log": []byte("deep"),
	}
	b, _, _ := newBrowserWithFake(t, files)

	paths, truncated, err := b.ListTree(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Errorf("should not be truncated under the cap")
	}
	if len(paths) != 4 {
		t.Fatalf("got %d paths, want 4: %v", len(paths), paths)
	}
	// Paths must be relative to the build root (prefix stripped), so they can
	// be passed straight to read/tail/grep.
	want := map[string]bool{
		"build-log.txt":       true,
		"started.json":        true,
		"artifacts/junit.xml": true,
		"artifacts/clusters/c1/machines/m1/boot.log": true,
	}
	for _, p := range paths {
		if !want[p] {
			t.Errorf("unexpected path %q (not build-root-relative?)", p)
		}
	}
}

func TestListTree_TruncatesAtCap(t *testing.T) {
	files := map[string][]byte{
		"logs/job1/b1/a.log": []byte("1"),
		"logs/job1/b1/b.log": []byte("2"),
		"logs/job1/b1/c.log": []byte("3"),
	}
	b, _, _ := newBrowserWithFake(t, files)
	paths, truncated, err := b.ListTree(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || !truncated {
		t.Errorf("got %d paths truncated=%v, want 2 truncated=true", len(paths), truncated)
	}
}
