// Package artifacts provides a read-only Browser over a single Prow build's
// GCS artifact tree. It is the abstraction the agentic AI loop calls into so
// that the AI loop stays GCS-agnostic and can be tested against fakes.
//
// One Browser instance is bound to one (bucket, job, build); reuse across
// builds is not supported. Construct a fresh Browser per build via a factory.
package artifacts

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Browser is a per-build view into the artifact tree. All paths are relative
// to the build root (i.e. immediately under logs/<job>/<build>/ in GCS).
//
// Implementations MUST enforce path safety: reject absolute paths, paths
// containing ".." segments, control characters, backslashes, or anything
// that looks like a URL. Callers may layer additional validation but MUST
// NOT rely on the caller to do so.
type Browser interface {
	// BuildRoot returns a human-readable identifier for the build being
	// browsed (e.g. "kubernetes-ci-logs/<job>/<build>"). Used for logging
	// and to seed the system prompt; not a URL.
	BuildRoot() string

	// List returns the immediate children (subdirectory names + file
	// metadata) of the given directory path. An empty path lists the
	// build root. Always returns paths relative to the build root.
	List(ctx context.Context, dir string) (*Listing, error)

	// Read fetches a byte range of a file. offset is 0-based; length is
	// the maximum number of bytes to return. The returned int64 is the
	// total file size (or -1 if the underlying store didn't report it).
	Read(ctx context.Context, file string, offset, length int) ([]byte, int64, error)

	// Tail returns the last N lines of a file without loading the whole
	// file into memory. Implementations should use a suffix-range fetch
	// when possible. Returned Content MUST NOT exceed maxBytes.
	Tail(ctx context.Context, file string, lines, maxBytes int) (*TailResult, error)

	// Grep streams the file and returns up to maxMatches matching lines
	// with the given amount of context. Implementations MUST stream
	// (not load the whole file into memory) so this is safe for very
	// large build-logs.
	Grep(ctx context.Context, file string, re *regexp.Regexp, contextLines, maxMatches, maxLineLen int) (*GrepResult, error)
}

// Factory creates per-build Browser instances. The factory is constructed
// once per fetcher run; individual Browsers are short-lived (one per build
// analyzed) so the underlying file cache stays bounded.
//
// buildPrefix is the bucket-relative directory of the build, always
// trailing-slashed (e.g. "logs/<job>/<build>/" for periodics or
// "pr-logs/pull/<org_repo>/<pr#>/<job>/<build>/" for presubmits). Callers
// typically build it with gcs.Bucket helpers (loc.BuildPath()).
// displayName is a human-readable label used for logging and seeded into
// the agentic AI system prompt (e.g. "<job>/<build>"); it is not a URL.
type Factory interface {
	ForBuild(buildPrefix, displayName string) Browser
}

// Listing is the result of Browser.List.
type Listing struct {
	// Dir is the directory that was listed, relative to the build root.
	// Always trailing-slashed except for the empty (root) directory.
	Dir string
	// Dirs lists immediate subdirectory names (without the parent prefix,
	// always trailing-slashed).
	Dirs []string
	// Files lists immediate child files.
	Files []FileInfo
	// Truncated indicates the listing was capped at the first page; more
	// entries exist but were not returned.
	Truncated bool
}

// FileInfo describes a single file in a Listing.
type FileInfo struct {
	Name string
	Size int64
}

// TailResult is the result of Browser.Tail.
type TailResult struct {
	// FileSize is the total size of the file in bytes, or -1 if unknown.
	FileSize int64
	// LinesReturned is the number of lines actually included in Content.
	LinesReturned int
	// Content is the tail of the file (last N lines, capped at maxBytes).
	Content []byte
}

// GrepResult is the result of Browser.Grep.
type GrepResult struct {
	FileSize int64
	// TotalMatches is the total number of matches found (may exceed the
	// number returned in Matches if maxMatches was hit).
	TotalMatches int
	Matches      []GrepMatch
	// Truncated indicates more matches existed than returned.
	Truncated bool
	// BytesScanned is the number of bytes streamed before stopping (may
	// be less than FileSize if a budget was exhausted).
	BytesScanned int64
}

// GrepMatch is one matching line with surrounding context.
type GrepMatch struct {
	// LineNo is 1-based.
	LineNo int
	// Context is the formatted context block; each entry is one line
	// already prefixed with "> " (match) or "  " (context) and a line
	// number for readability.
	Context []string
}

// ---------- Path validation ----------

// ErrUnsafePath is returned when a caller-supplied path fails validation.
var ErrUnsafePath = errors.New("unsafe path")

var pathDangerousRe = regexp.MustCompile(`[\x00-\x1f\\]`)

// SafePath validates a user-supplied path and returns its cleaned form
// relative to the build root. The returned path never starts with "/"
// and never contains "..". An empty input returns ("", nil) which
// implementations should treat as the build root.
//
// Implementations SHOULD call this on every public path argument.
func SafePath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if strings.ContainsRune(p, '\x00') {
		return "", fmt.Errorf("%w: NUL byte", ErrUnsafePath)
	}
	if pathDangerousRe.MatchString(p) {
		return "", fmt.Errorf("%w: control character or backslash", ErrUnsafePath)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w: must be relative to build root", ErrUnsafePath)
	}
	if strings.Contains(p, "://") {
		return "", fmt.Errorf("%w: looks like a URL", ErrUnsafePath)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: contains .. segment", ErrUnsafePath)
		}
	}
	cleaned := path.Clean("/" + p)
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "." {
		return "", nil
	}
	return cleaned, nil
}
