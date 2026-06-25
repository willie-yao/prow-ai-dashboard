package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// gcsweb caching bounds. gcsweb serves whole objects (no HTTP Range), so we
// cache fetched bodies to avoid re-downloading the same log for successive
// ranged/tail/grep reads within a run. Files above cacheFileCap are never
// cached; caching stops once cacheBudget total bytes are resident.
const (
	cacheFileCap = 16 * 1024 * 1024  // 16 MB
	cacheBudget  = 256 * 1024 * 1024 // 256 MB per run
)

// gcswebBackend reads through a gcsweb HTTP gateway (e.g. gcsweb.istio.io/s3)
// that fronts a bucket. The same URL serves a raw object (no trailing slash)
// or an HTML directory listing (trailing slash). HTTP Range is not assumed, so
// ranged and tail reads fetch the whole object and slice it.
type gcswebBackend struct {
	bucket   string
	client   *http.Client
	base     string // "https://gcsweb.istio.io/s3"
	webBase  string
	prowBase string

	mu    sync.Mutex
	cache map[string][]byte
	used  int64
}

func newGCSWebBackend(cfg Config, client *http.Client) *gcswebBackend {
	webBase := cfg.WebBase
	if webBase == "" {
		webBase = cfg.Base
	}
	prowBase := cfg.ProwBase
	if prowBase == "" {
		prowBase = webBase
	}
	return &gcswebBackend{
		bucket:   cfg.Bucket,
		client:   client,
		base:     strings.TrimRight(cfg.Base, "/"),
		webBase:  webBase,
		prowBase: prowBase,
		cache:    map[string][]byte{},
	}
}

// objURL is the raw-object URL (no trailing slash).
func (b *gcswebBackend) objURL(path string) string {
	return b.base + "/" + b.bucket + "/" + escapePath(strings.TrimLeft(path, "/"))
}

// dirURL is the directory-listing URL (trailing slash).
func (b *gcswebBackend) dirURL(prefix string) string {
	return b.base + "/" + b.bucket + "/" + escapePath(strings.TrimLeft(prefix, "/"))
}

func (b *gcswebBackend) cacheGet(path string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.cache[path]
	return v, ok
}

func (b *gcswebBackend) cachePut(path string, body []byte) {
	if int64(len(body)) > cacheFileCap {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.cache[path]; ok {
		return
	}
	if b.used+int64(len(body)) > cacheBudget {
		return
	}
	b.cache[path] = append([]byte(nil), body...)
	b.used += int64(len(body))
}

// whole returns the entire object body, from cache when resident.
func (b *gcswebBackend) whole(ctx context.Context, path string) ([]byte, error) {
	if v, ok := b.cacheGet(path); ok {
		return v, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.objURL(path), nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch %s: HTTP %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, perCallCap))
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", path, err)
	}
	b.cachePut(path, body)
	return body, nil
}

func (b *gcswebBackend) Open(ctx context.Context, path string) (io.ReadCloser, int64, error) {
	body, err := b.whole(ctx, path)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
}

// streamCap bounds how many bytes a single streaming read will consume from a
// gcsweb object, a guard against a runaway/very large object. Tail/range reads
// past this are best-effort.
const streamCap = 1 << 30 // 1 GiB

// get issues the raw object GET and returns the response (caller closes Body)
// plus the Content-Length (-1 if absent).
func (b *gcswebBackend) get(ctx context.Context, path string) (*http.Response, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.objURL(path), nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("fetch %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("fetch %s: HTTP %d", path, resp.StatusCode)
	}
	total := int64(-1)
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			total = n
		}
	}
	return resp, total, nil
}

func (b *gcswebBackend) ReadRange(ctx context.Context, path string, offset, length int64) ([]byte, int64, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("read %s: offset must be >= 0", path)
	}
	if length <= 0 {
		return nil, 0, fmt.Errorf("read %s: length must be > 0", path)
	}
	if cached, ok := b.cacheGet(path); ok {
		return sliceRange(cached, offset, length), int64(len(cached)), nil
	}
	resp, total, err := b.get(ctx, path)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	// Small enough to buffer whole: read, cache, slice.
	if total >= 0 && total <= cacheFileCap {
		body, err := io.ReadAll(io.LimitReader(resp.Body, cacheFileCap))
		if err != nil {
			return nil, 0, fmt.Errorf("read %s: %w", path, err)
		}
		b.cachePut(path, body)
		return sliceRange(body, offset, length), int64(len(body)), nil
	}
	// Large object: stream past offset, then read length bytes.
	if _, err := io.CopyN(io.Discard, resp.Body, offset); err != nil {
		// offset is past EOF (or a short object): nothing to return.
		return nil, total, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, length))
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", path, err)
	}
	return data, total, nil
}

func (b *gcswebBackend) ReadTail(ctx context.Context, path string, maxBytes int64) ([]byte, int64, error) {
	if maxBytes <= 0 {
		maxBytes = perCallCap
	}
	if cached, ok := b.cacheGet(path); ok {
		return tailOf(cached, maxBytes), int64(len(cached)), nil
	}
	resp, total, err := b.get(ctx, path)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if total >= 0 && total <= cacheFileCap {
		body, err := io.ReadAll(io.LimitReader(resp.Body, cacheFileCap))
		if err != nil {
			return nil, 0, fmt.Errorf("tail %s: %w", path, err)
		}
		b.cachePut(path, body)
		return tailOf(body, maxBytes), int64(len(body)), nil
	}
	// Large object: stream, keeping only the last maxBytes.
	data, streamed := streamTail(resp.Body, maxBytes)
	if total < 0 {
		total = streamed
	}
	return data, total, nil
}

// sliceRange returns body[offset:offset+length] clamped to body.
func sliceRange(body []byte, offset, length int64) []byte {
	total := int64(len(body))
	if offset >= total {
		return nil
	}
	end := offset + length
	if end > total {
		end = total
	}
	return body[offset:end]
}

// tailOf returns the last maxBytes of body.
func tailOf(body []byte, maxBytes int64) []byte {
	if int64(len(body)) > maxBytes {
		return body[int64(len(body))-maxBytes:]
	}
	return body
}

// streamTail reads r (up to streamCap bytes) keeping only the last maxBytes,
// returning that tail and the total bytes seen.
func streamTail(r io.Reader, maxBytes int64) ([]byte, int64) {
	buf := make([]byte, 0, maxBytes)
	tmp := make([]byte, 64*1024)
	var total int64
	for total < streamCap {
		n, err := r.Read(tmp)
		if n > 0 {
			total += int64(n)
			buf = append(buf, tmp[:n]...)
			if int64(len(buf)) > maxBytes {
				buf = buf[int64(len(buf))-maxBytes:]
			}
		}
		if err != nil {
			break
		}
	}
	return buf, total
}

// hrefRe extracts the href targets from a gcsweb listing page.
var hrefRe = regexp.MustCompile(`href="([^"]+)"`)

func (b *gcswebBackend) List(ctx context.Context, prefix string) (*Listing, error) {
	listURL := b.dirURL(prefix)
	if !strings.HasSuffix(listURL, "/") {
		listURL += "/"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", prefix, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list %s: HTTP %d", prefix, resp.StatusCode)
	}
	html, err := io.ReadAll(io.LimitReader(resp.Body, perCallCap))
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", prefix, err)
	}
	// The listing's own path under the gcsweb route, decoded. Child entries
	// are hrefs that extend it by exactly one segment.
	u, err := url.Parse(listURL)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", prefix, err)
	}
	basePath := u.Path // e.g. "/s3/<bucket>/<prefix>/"

	out := &Listing{}
	seen := map[string]bool{}
	for _, m := range hrefRe.FindAllStringSubmatch(string(html), -1) {
		href, err := url.PathUnescape(m[1])
		if err != nil {
			continue
		}
		if !strings.HasPrefix(href, basePath) {
			continue // stylesheet, icon, or the ".." parent link
		}
		child := strings.TrimPrefix(href, basePath)
		if child == "" || seen[child] {
			continue
		}
		// Immediate children only: one segment, optionally trailing-slashed.
		trimmed := strings.TrimSuffix(child, "/")
		if strings.Contains(trimmed, "/") {
			continue
		}
		seen[child] = true
		if strings.HasSuffix(child, "/") {
			out.Dirs = append(out.Dirs, child)
		} else {
			out.Files = append(out.Files, Object{Name: child})
		}
	}
	return out, nil
}

// maxTreeDirs caps the directories ListTree will descend into, a guard against
// pathologically deep trees since gcsweb has no recursive listing.
const maxTreeDirs = 500

func (b *gcswebBackend) ListTree(ctx context.Context, prefix string, max int) ([]string, bool, error) {
	if max <= 0 {
		return nil, false, nil
	}
	prefix = strings.TrimRight(prefix, "/") + "/"
	var out []string
	queue := []string{""} // sub-prefixes relative to the root prefix
	dirsVisited := 0
	for len(queue) > 0 {
		rel := queue[0]
		queue = queue[1:]
		dirsVisited++
		if dirsVisited > maxTreeDirs {
			return out, true, nil
		}
		listing, err := b.List(ctx, prefix+rel)
		if err != nil {
			return nil, false, err
		}
		for _, f := range listing.Files {
			out = append(out, rel+f.Name)
			if len(out) >= max {
				return out, true, nil
			}
		}
		for _, d := range listing.Dirs {
			queue = append(queue, rel+d)
		}
	}
	return out, false, nil
}

func (b *gcswebBackend) WebURL(path string) string {
	return joinURL(b.webBase, b.bucket, path)
}

func (b *gcswebBackend) ProwURL(path string) string {
	return joinURL(b.prowBase, b.bucket, path)
}
