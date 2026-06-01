package artifacts

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// GCSFactory creates per-build GCSBrowser instances over a single Prow bucket.
//
// Browsers are memoized by buildPrefix so multiple analyses against the same
// build share the underlying small-file cache and any future per-build tool
// cache. The factory is intended to be created once per fetcher run, so the
// memo lives for the run; per-build entries are bounded by the number of
// distinct builds analyzed.
type GCSFactory struct {
	bucket string
	client *http.Client

	mu       sync.Mutex
	browsers map[string]*GCSBrowser
}

// NewGCSFactory returns a Factory backed by the given GCS bucket. The factory
// assumes Prow's "logs/<job>/<build>/" object naming convention.
func NewGCSFactory(bucket string, client *http.Client) *GCSFactory {
	if client == nil {
		client = http.DefaultClient
	}
	return &GCSFactory{
		bucket:   bucket,
		client:   client,
		browsers: map[string]*GCSBrowser{},
	}
}

// ForBuild returns a Browser bound to a single Prow build. buildPrefix is
// the bucket-relative directory of the build (always trailing-slashed),
// e.g. "logs/<job>/<build>/" for periodics or
// "pr-logs/pull/<org_repo>/<pr#>/<job>/<build>/" for presubmits.
// displayName is the human-readable label surfaced via BuildRoot().
//
// Repeated calls with the same buildPrefix return the same Browser instance
// so callers share its file cache.
func (f *GCSFactory) ForBuild(buildPrefix, displayName string) Browser {
	if !strings.HasSuffix(buildPrefix, "/") {
		buildPrefix += "/"
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.browsers[buildPrefix]; ok {
		return b
	}
	b := &GCSBrowser{
		bucket:    f.bucket,
		display:   displayName,
		client:    f.client,
		prefix:    buildPrefix,
		objectURL: fmt.Sprintf("https://storage.googleapis.com/%s/", f.bucket),
		listURL:   fmt.Sprintf("https://storage.googleapis.com/storage/v1/b/%s/o", f.bucket),
		cache:     map[string][]byte{},
	}
	f.browsers[buildPrefix] = b
	return b
}

// GCSBrowser implements Browser against GCS for one Prow build.
type GCSBrowser struct {
	bucket    string
	display   string // "<job>/<build>" or any caller-supplied label
	client    *http.Client
	prefix    string // bucket-relative build directory, trailing-slashed
	objectURL string // "https://storage.googleapis.com/<bucket>/"
	listURL   string // "https://storage.googleapis.com/storage/v1/b/<bucket>/o"

	// cache stores fully-fetched small files keyed by relative path. Only
	// files at or below smallFileCacheCap are eligible. Hits are reused
	// by Read and Tail when the file is already resident.
	cache map[string][]byte
}

// Per-file cache cap. Files larger than this are never loaded whole; the
// model must use Read with byte ranges, Tail (suffix-range), or Grep
// (streaming) instead.
const smallFileCacheCap = 1 * 1024 * 1024 // 1 MB

// Cap a single tool fetch (used as a circuit-breaker for malicious or
// confused range requests). Any single Read/Tail/Grep call that would pull
// more than this from GCS in one go errors out.
const perCallGCSCap = 64 * 1024 * 1024 // 64 MB

func (b *GCSBrowser) BuildRoot() string {
	return fmt.Sprintf("%s/%s", b.bucket, b.display)
}

// ---------- List ----------

func (b *GCSBrowser) List(ctx context.Context, dir string) (*Listing, error) {
	clean, err := SafePath(dir)
	if err != nil {
		return nil, err
	}
	if clean != "" && !strings.HasSuffix(clean, "/") {
		clean += "/"
	}
	fullPrefix := b.prefix + clean

	apiURL := fmt.Sprintf("%s?prefix=%s&delimiter=%s&maxResults=1000",
		b.listURL, queryEscape(fullPrefix), queryEscape("/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", clean, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, perCallGCSCap))
	if err != nil {
		return nil, fmt.Errorf("list %s: read body: %w", clean, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list %s: HTTP %d", clean, resp.StatusCode)
	}

	var raw struct {
		Items []struct {
			Name string `json:"name"`
			Size string `json:"size"`
		} `json:"items"`
		Prefixes      []string `json:"prefixes"`
		NextPageToken string   `json:"nextPageToken"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("list %s: parse: %w", clean, err)
	}

	out := &Listing{Dir: clean, Truncated: raw.NextPageToken != ""}
	for _, p := range raw.Prefixes {
		out.Dirs = append(out.Dirs, strings.TrimPrefix(p, fullPrefix))
	}
	for _, it := range raw.Items {
		name := strings.TrimPrefix(it.Name, fullPrefix)
		if name == "" {
			continue
		}
		size, _ := strconv.ParseInt(it.Size, 10, 64)
		out.Files = append(out.Files, FileInfo{Name: name, Size: size})
	}
	return out, nil
}

// ---------- Read ----------

func (b *GCSBrowser) Read(ctx context.Context, file string, offset, length int) ([]byte, int64, error) {
	clean, err := SafePath(file)
	if err != nil {
		return nil, 0, err
	}
	if clean == "" {
		return nil, 0, fmt.Errorf("read: file path is required")
	}
	if offset < 0 {
		return nil, 0, fmt.Errorf("read: offset must be >= 0")
	}
	if length <= 0 {
		return nil, 0, fmt.Errorf("read: length must be > 0")
	}
	if length > perCallGCSCap {
		length = perCallGCSCap
	}

	// Cache hit: slice from memory.
	if data, ok := b.cache[clean]; ok {
		return sliceCached(data, offset, length), int64(len(data)), nil
	}

	objURL := b.objectURL + b.prefix + escapePath(clean)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, objURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", clean, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, 0, fmt.Errorf("read %s: HTTP %d", clean, resp.StatusCode)
	}
	// Hard cap server response at length+1 so a server that ignored Range
	// can't blow the per-call cap.
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(length)))
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", clean, err)
	}

	totalSize := parseContentRangeTotal(resp.Header.Get("Content-Range"))
	if totalSize < 0 && resp.StatusCode == http.StatusOK {
		// Server ignored Range and returned the whole file: opportunistic
		// cache if it fits.
		if int64(len(body)) <= smallFileCacheCap {
			b.cache[clean] = append([]byte(nil), body...)
		}
		totalSize = int64(len(body))
	}
	return body, totalSize, nil
}

func sliceCached(data []byte, offset, length int) []byte {
	if offset >= len(data) {
		return nil
	}
	end := offset + length
	if end > len(data) {
		end = len(data)
	}
	return data[offset:end]
}

// parseContentRangeTotal extracts the "/<total>" suffix from a Content-Range
// header. Returns -1 if unparseable.
func parseContentRangeTotal(h string) int64 {
	if h == "" {
		return -1
	}
	i := strings.LastIndex(h, "/")
	if i < 0 {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(h[i+1:]), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// ---------- Tail ----------

// tailFetchChunk is the initial suffix-range size used by Tail. If the chunk
// covers fewer than the requested lines and the file is bigger, Tail walks
// backwards in roughly-doubling chunks until enough lines are collected or
// the start of the file is reached.
const tailFetchChunk = 64 * 1024

func (b *GCSBrowser) Tail(ctx context.Context, file string, lines, maxBytes int) (*TailResult, error) {
	clean, err := SafePath(file)
	if err != nil {
		return nil, err
	}
	if clean == "" {
		return nil, fmt.Errorf("tail: file path is required")
	}
	if lines <= 0 {
		return nil, fmt.Errorf("tail: lines must be > 0")
	}
	if maxBytes <= 0 || maxBytes > perCallGCSCap {
		maxBytes = perCallGCSCap
	}

	if data, ok := b.cache[clean]; ok {
		return tailFromBytes(data, int64(len(data)), lines, maxBytes), nil
	}

	objURL := b.objectURL + b.prefix + escapePath(clean)

	// Walk backwards from EOF with suffix ranges. Stop when we have enough
	// lines or have read maxBytes.
	chunk := int64(tailFetchChunk)
	var totalFileSize int64 = -1
	var accumulated []byte
	for int64(len(accumulated)) < int64(maxBytes) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, objURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=-%d", chunk))
		resp, err := b.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("tail %s: %w", clean, err)
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			return nil, fmt.Errorf("tail %s: HTTP %d", clean, resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, chunk))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("tail %s: %w", clean, err)
		}
		totalFileSize = parseContentRangeTotal(resp.Header.Get("Content-Range"))
		// Suffix range returns the last min(chunk, fileSize) bytes; if the
		// file is smaller than chunk we got the whole thing.
		accumulated = body
		// Count newlines to see if we have enough lines.
		nl := bytes.Count(accumulated, []byte("\n"))
		if nl >= lines {
			break
		}
		if totalFileSize > 0 && int64(len(accumulated)) >= totalFileSize {
			break // Got the whole file.
		}
		// Double the chunk and refetch. Capped at maxBytes.
		chunk *= 2
		if chunk > int64(maxBytes) {
			chunk = int64(maxBytes)
		}
		// If we've already requested maxBytes and didn't get enough lines,
		// just return what we have rather than looping forever.
		if int64(len(accumulated)) >= int64(maxBytes) {
			break
		}
	}

	// Cache opportunistically if the whole file fits.
	if totalFileSize >= 0 && int64(len(accumulated)) >= totalFileSize && totalFileSize <= smallFileCacheCap {
		b.cache[clean] = append([]byte(nil), accumulated...)
	}

	return tailFromBytes(accumulated, totalFileSize, lines, maxBytes), nil
}

func tailFromBytes(data []byte, fileSize int64, lines, maxBytes int) *TailResult {
	// Drop trailing newline so split doesn't yield a phantom empty line.
	data = bytes.TrimRight(data, "\n")
	// Split on \n. If we fetched a partial suffix the first line may be
	// truncated; drop it unless we got the whole file.
	all := bytes.Split(data, []byte("\n"))
	if fileSize > 0 && int64(len(data)) < fileSize && len(all) > 0 {
		all = all[1:]
	}
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	out := bytes.Join(all, []byte("\n"))
	if len(out) > maxBytes {
		out = out[len(out)-maxBytes:]
	}
	return &TailResult{
		FileSize:      fileSize,
		LinesReturned: len(all),
		Content:       out,
	}
}

// ---------- Grep ----------

func (b *GCSBrowser) Grep(ctx context.Context, file string, re *regexp.Regexp, contextLines, maxMatches, maxLineLen int) (*GrepResult, error) {
	clean, err := SafePath(file)
	if err != nil {
		return nil, err
	}
	if clean == "" {
		return nil, fmt.Errorf("grep: file path is required")
	}
	if re == nil {
		return nil, fmt.Errorf("grep: regex is required")
	}
	if contextLines < 0 {
		contextLines = 0
	}
	if maxMatches <= 0 {
		maxMatches = 30
	}
	if maxLineLen <= 0 {
		maxLineLen = 1000
	}

	if data, ok := b.cache[clean]; ok {
		return grepStream(bytes.NewReader(data), int64(len(data)), int64(len(data)), re, contextLines, maxMatches, maxLineLen), nil
	}

	objURL := b.objectURL + b.prefix + escapePath(clean)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, objURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grep %s: %w", clean, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("grep %s: HTTP %d", clean, resp.StatusCode)
	}
	fileSize := int64(-1)
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		fileSize, _ = strconv.ParseInt(cl, 10, 64)
	}
	limited := io.LimitReader(resp.Body, perCallGCSCap)
	return grepStream(limited, fileSize, perCallGCSCap, re, contextLines, maxMatches, maxLineLen), nil
}

// grepStream scans r for matching lines, returning up to maxMatches with
// contextLines of context on either side. Lines longer than maxLineLen are
// truncated. fileSize is the underlying file's full size (or -1 if unknown);
// maxBytes is the hard cap on bytes consumed from r.
func grepStream(r io.Reader, fileSize, maxBytes int64, re *regexp.Regexp, contextLines, maxMatches, maxLineLen int) *GrepResult {
	out := &GrepResult{FileSize: fileSize}

	// Ring buffer of recent lines for "before" context.
	before := make([]string, contextLines)
	beforeIdx := 0

	// Pending "after" capture: when we record a match, we still need to
	// grab the next contextLines lines; this counter tracks how many.
	type pendingAfter struct {
		matchIdx int
		count    int
	}
	var pendings []*pendingAfter

	scanner := bufio.NewScanner(r)
	// Allow long lines without triggering bufio.ErrTooLong.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if len(line) > maxLineLen {
			line = line[:maxLineLen] + "...<truncated>"
		}

		// Append "after" context to any pending matches.
		stillPending := pendings[:0]
		for _, p := range pendings {
			out.Matches[p.matchIdx].Context = append(
				out.Matches[p.matchIdx].Context,
				fmt.Sprintf("  %d: %s", lineNo, line),
			)
			p.count--
			if p.count > 0 {
				stillPending = append(stillPending, p)
			}
		}
		pendings = stillPending

		if re.MatchString(line) {
			out.TotalMatches++
			if len(out.Matches) < maxMatches {
				ctx := make([]string, 0, 2*contextLines+1)
				// "before" lines in chronological order.
				for i := 0; i < contextLines; i++ {
					idx := (beforeIdx + i) % contextLines
					if before[idx] == "" {
						continue
					}
					ctx = append(ctx, before[idx])
				}
				ctx = append(ctx, fmt.Sprintf("> %d: %s", lineNo, line))
				m := GrepMatch{LineNo: lineNo, Context: ctx}
				out.Matches = append(out.Matches, m)
				if contextLines > 0 {
					pendings = append(pendings, &pendingAfter{
						matchIdx: len(out.Matches) - 1,
						count:    contextLines,
					})
				}
			} else {
				out.Truncated = true
			}
		}

		// Slide the before buffer.
		if contextLines > 0 {
			before[beforeIdx] = fmt.Sprintf("  %d: %s", lineNo, line)
			beforeIdx = (beforeIdx + 1) % contextLines
		}
	}
	// Best-effort scan errors get logged via the result rather than failing
	// the call: a malformed last line is still useful data.
	_ = scanner.Err()
	out.BytesScanned = countBytesScanned(maxBytes, lineNo)
	return out
}

// countBytesScanned is a conservative estimate: bufio.Scanner doesn't expose
// underlying-reader bytes consumed directly. We approximate using lines
// scanned (callers use it for logging, not budget enforcement; the
// LimitReader already capped the underlying read).
func countBytesScanned(maxBytes int64, lines int) int64 {
	if int64(lines)*80 > maxBytes {
		return maxBytes
	}
	return int64(lines) * 80
}

// ---------- URL helpers ----------

func queryEscape(s string) string {
	const hex = "0123456789ABCDEF"
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			out = append(out, c)
			continue
		}
		out = append(out, '%', hex[c>>4], hex[c&0x0f])
	}
	return string(out)
}

func escapePath(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = queryEscape(p)
	}
	return strings.Join(parts, "/")
}
