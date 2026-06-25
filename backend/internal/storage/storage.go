// Package storage abstracts read access to a Prow build-artifact store so the
// engine does not assume Google Cloud Storage. A Backend exposes the two
// primitives every caller needs, object reads (whole, ranged, or tail) and
// prefix listings, plus the human-facing URL templates for a build.
//
// Two providers implement Backend:
//
//   - gcs:    native Google Cloud Storage (storage.googleapis.com object reads
//     with HTTP Range, the GCS JSON list API). Used by kubernetes.io Prow.
//   - gcsweb: any gcsweb HTTP gateway in front of a bucket (e.g. an S3 bucket
//     fronted by gcsweb.istio.io). Raw object reads and HTML directory
//     listings; HTTP Range is not assumed, so ranged reads are emulated by
//     fetching the whole object and slicing.
//
// All Backend paths are bucket-relative (no leading slash, no bucket prefix),
// e.g. "logs/<job>/<build>/started.json".
package storage

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// Provider names a storage backend implementation.
type Provider string

const (
	ProviderGCS    Provider = "gcs"
	ProviderGCSWeb Provider = "gcsweb"
)

// Config selects and configures a Backend. Bucket is always required. The
// *Base fields are optional overrides; each provider supplies sane defaults
// (the kubernetes.io endpoints for gcs, and Base itself for gcsweb).
type Config struct {
	Provider Provider
	Bucket   string
	// Base is the gcsweb gateway root that serves both raw objects and HTML
	// listings, e.g. "https://gcsweb.istio.io/s3". Required for the gcsweb
	// provider; ignored by gcs.
	Base string
	// WebBase is the root for human-browsable build links. Defaults to
	// "https://gcsweb.k8s.io/gcs" (gcs) or Base (gcsweb).
	WebBase string
	// ProwBase is the root for Prow "deck" deep links, e.g.
	// "https://prow.k8s.io/view/gs" (gcs default) or
	// "https://prow.istio.io/view/s3" (gcsweb). Defaults to WebBase when
	// unset for gcsweb.
	ProwBase string
}

// Listing is the result of Backend.List: the immediate children of a prefix.
type Listing struct {
	// Dirs holds subdirectory names, each trailing-slashed and relative to
	// the listed prefix.
	Dirs []string
	// Files holds the immediate file children relative to the listed prefix.
	Files []Object
	// Truncated is true when the listing was capped before exhaustion.
	Truncated bool
}

// Object is a single file entry in a Listing. Size is the byte size, or 0 when
// the backend cannot report it (the gcsweb HTML listing omits sizes).
type Object struct {
	Name string
	Size int64
}

// Backend is read-only access to one Prow artifact bucket. Implementations are
// safe for concurrent use. Paths are bucket-relative.
type Backend interface {
	// Open returns a reader over the object plus its total size (-1 if
	// unknown). The caller must Close the reader.
	Open(ctx context.Context, path string) (io.ReadCloser, int64, error)

	// ReadRange returns up to length bytes starting at offset, plus the
	// object's total size (-1 if unknown).
	ReadRange(ctx context.Context, path string, offset, length int64) ([]byte, int64, error)

	// ReadTail returns up to maxBytes from the end of the object, plus the
	// total size (-1 if unknown).
	ReadTail(ctx context.Context, path string, maxBytes int64) ([]byte, int64, error)

	// List returns the immediate children of prefix (delimiter listing),
	// paginating internally. prefix should be trailing-slashed.
	List(ctx context.Context, prefix string) (*Listing, error)

	// ListTree returns up to max object paths under prefix (recursive),
	// relative to prefix. truncated is true when more objects exist.
	ListTree(ctx context.Context, prefix string, max int) (paths []string, truncated bool, err error)

	// WebURL returns the human-browsable URL for a bucket-relative path.
	WebURL(path string) string

	// ProwURL returns the Prow deck URL for a bucket-relative path.
	ProwURL(path string) string
}

// New builds a Backend from cfg. client may be nil (http.DefaultClient is
// used). It validates that required fields for the chosen provider are set.
func New(cfg Config, client *http.Client) (Backend, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("storage: bucket is required")
	}
	switch cfg.Provider {
	case ProviderGCS:
		return newGCSBackend(cfg, client), nil
	case ProviderGCSWeb:
		if cfg.Base == "" {
			return nil, fmt.Errorf("storage: gcsweb provider requires a base URL")
		}
		return newGCSWebBackend(cfg, client), nil
	case "":
		return nil, fmt.Errorf("storage: provider is required (gcs or gcsweb)")
	default:
		return nil, fmt.Errorf("storage: unknown provider %q (want gcs or gcsweb)", cfg.Provider)
	}
}

// ReadAll reads the whole object at path via the backend.
func ReadAll(ctx context.Context, b Backend, path string) ([]byte, error) {
	rc, _, err := b.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
