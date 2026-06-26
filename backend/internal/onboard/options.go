package onboard

// Options configures a scaffold run. One discovery selector (TestGrid or
// Bucket) is required, plus the dashboard and source repos.
type Options struct {
	// TestGrid is the testgrid-dashboards annotation value (kubernetes-ecosystem
	// Prow). Mutually exclusive with Bucket.
	TestGrid string
	// Bucket is the artifact bucket name for bucket-based discovery (any Prow).
	Bucket string
	// GCSWebBase, when set with Bucket, selects the gcsweb provider and is the
	// gateway root (e.g. https://gcsweb.istio.io/s3). Empty means native gcs.
	GCSWebBase string

	// DashboardRepo is "owner/name" of the repo that will publish the dashboard
	// (drives branding.base_path and site_url).
	DashboardRepo string
	// SourceRepo is "owner/name" of the code under test (branding.source_repo).
	SourceRepo string

	// ID/Name override the derived project identity. Optional.
	ID   string
	Name string

	// IncludePresubmits widens the sweep to presubmit jobs.
	IncludePresubmits bool

	// EngineRef is the prow-ai-dashboard ref the generated workflows pin
	// (defaults to "main").
	EngineRef string

	// OutDir is where the scaffold is written.
	OutDir string
}
