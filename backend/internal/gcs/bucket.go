package gcs

import "fmt"

// Bucket centralizes URL construction for a GCS bucket that holds Prow
// build logs. Every helper assumes Prow's "logs/" prefix convention so
// callers can pass paths like "jobname/buildid/build-log.txt".
type Bucket struct {
	Name string
}

// NewBucket returns a Bucket helper for the given GCS bucket.
func NewBucket(name string) *Bucket { return &Bucket{Name: name} }

// ObjectURL returns the raw GCS object URL for the given path under logs/.
//
//	NewBucket("kubernetes-ci-logs").ObjectURL("foo/1/build-log.txt") ->
//	  https://storage.googleapis.com/kubernetes-ci-logs/logs/foo/1/build-log.txt
func (b *Bucket) ObjectURL(path string) string {
	return fmt.Sprintf("https://storage.googleapis.com/%s/logs/%s", b.Name, path)
}

// ObjectBaseURL returns the raw GCS prefix for the given path under logs/,
// always trailing-slashed. Useful when callers want to append filenames.
func (b *Bucket) ObjectBaseURL(path string) string {
	return fmt.Sprintf("https://storage.googleapis.com/%s/logs/%s", b.Name, ensureSlash(path))
}

// WebURL returns the human-browsable GCSweb URL for the given path under logs/.
func (b *Bucket) WebURL(path string) string {
	return fmt.Sprintf("https://gcsweb.k8s.io/gcs/%s/logs/%s", b.Name, path)
}

// ProwURL returns the Prow UI URL for the given path under logs/.
func (b *Bucket) ProwURL(path string) string {
	return fmt.Sprintf("https://prow.k8s.io/view/gs/%s/logs/%s", b.Name, path)
}

// ListAPIURL returns the GCS JSON API endpoint for listing objects in this bucket.
func (b *Bucket) ListAPIURL() string {
	return fmt.Sprintf("https://storage.googleapis.com/storage/v1/b/%s/o", b.Name)
}

func ensureSlash(s string) string {
	if s == "" {
		return ""
	}
	if s[len(s)-1] == '/' {
		return s
	}
	return s + "/"
}
