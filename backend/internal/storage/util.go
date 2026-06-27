package storage

import "strings"

// queryEscape percent-encodes a URL path or query component.
// Spaces become %20, and escapePath handles slash-separated segments.
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

// escapePath percent-encodes each segment of a slash-separated path while
// preserving the slashes.
func escapePath(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = queryEscape(p)
	}
	return strings.Join(parts, "/")
}

// joinURL joins a base URL with bucket-relative path segments.
// A trailing slash on the final segment is preserved for directory bases.
func joinURL(base string, parts ...string) string {
	out := strings.TrimRight(base, "/")
	trailing := false
	for _, p := range parts {
		if p == "" {
			continue
		}
		trailing = strings.HasSuffix(p, "/")
		out += "/" + strings.Trim(p, "/")
	}
	if trailing {
		out += "/"
	}
	return out
}

// trimTotal extracts the "/<total>" suffix from a Content-Range header value.
// Returns -1 if absent or unparseable.
func trimTotal(contentRange string) int64 {
	if contentRange == "" {
		return -1
	}
	i := strings.LastIndex(contentRange, "/")
	if i < 0 {
		return -1
	}
	return parseInt64(strings.TrimSpace(contentRange[i+1:]))
}

// parseInt64 parses a base-10 int64, returning -1 on error.
func parseInt64(s string) int64 {
	if s == "" {
		return -1
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int64(r-'0')
	}
	return n
}
