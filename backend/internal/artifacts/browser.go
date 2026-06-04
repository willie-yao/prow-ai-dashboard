// Package artifacts provides a read-only Browser over a single Prow build's
// GCS artifact tree. One Browser instance is bound to one (bucket, job,
// build); construct a fresh Browser per build via a Factory.
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
// to the build root. Implementations MUST enforce path safety: reject
// absolute paths, ".." segments, control characters, backslashes, or URLs.
type Browser interface {
	// BuildRoot returns a human-readable identifier for the build (used
	// for logging and as a system-prompt seed; not a URL).
	BuildRoot() string

	// List returns the immediate children of dir, paths relative to the
	// build root. Empty dir lists the build root.
	List(ctx context.Context, dir string) (*Listing, error)

	// Read fetches a byte range of a file. Returns the bytes plus the
	// total file size (or -1 if unknown).
	Read(ctx context.Context, file string, offset, length int) ([]byte, int64, error)

	// Tail returns the last lines of a file using a suffix-range fetch
	// when possible. Returned Content MUST NOT exceed maxBytes.
	Tail(ctx context.Context, file string, lines, maxBytes int) (*TailResult, error)

	// Grep streams the file and returns up to maxMatches matching lines
	// with the given context. Implementations MUST stream rather than
	// load the whole file into memory.
	Grep(ctx context.Context, file string, re *regexp.Regexp, contextLines, maxMatches, maxLineLen int) (*GrepResult, error)
}

// Factory creates per-build Browser instances.
//
// buildPrefix is the bucket-relative, trailing-slashed directory of the
// build (e.g. "logs/<job>/<build>/"). displayName is a human-readable label
// for logging and the agentic system prompt (not a URL).
type Factory interface {
	ForBuild(buildPrefix, displayName string) Browser
}

// Listing is the result of Browser.List.
type Listing struct {
	// Dir is the directory that was listed (trailing-slashed except root).
	Dir string
	// Dirs lists subdirectory names, trailing-slashed.
	Dirs  []string
	Files []FileInfo
	// Truncated indicates the listing was capped at the first page.
	Truncated bool
}

// FileInfo describes a single file in a Listing.
type FileInfo struct {
	Name string
	Size int64
}

// TailResult is the result of Browser.Tail.
type TailResult struct {
	// FileSize is the total size of the file, or -1 if unknown.
	FileSize int64
	// LinesReturned is the number of lines actually included in Content.
	LinesReturned int
	Content       []byte
}

// GrepResult is the result of Browser.Grep.
type GrepResult struct {
	FileSize int64
	// TotalMatches may exceed len(Matches) if maxMatches was hit.
	TotalMatches int
	Matches      []GrepMatch
	Truncated    bool
	// BytesScanned may be less than FileSize if a budget was exhausted.
	BytesScanned int64
}

// GrepMatch is one matching line with surrounding context.
type GrepMatch struct {
	// LineNo is 1-based.
	LineNo int
	// Context lines already prefixed with "> " (match) or "  " (context).
	Context []string
}

// ---------- Path validation ----------

// ErrUnsafePath is returned when a caller-supplied path fails validation.
var ErrUnsafePath = errors.New("unsafe path")

var pathDangerousRe = regexp.MustCompile(`[\x00-\x1f\\]`)

// SafePath validates a user-supplied path and returns its cleaned form
// relative to the build root. The returned path never starts with "/" and
// never contains "..". Empty input returns ("", nil) which implementations
// should treat as the build root. Implementations SHOULD call this on
// every public path argument.
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
