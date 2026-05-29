package gcs

import (
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestBucketURLs(t *testing.T) {
	b := NewBucket("kubernetes-ci-logs")

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"ObjectURL", b.ObjectURL("job/1/build-log.txt"),
			"https://storage.googleapis.com/kubernetes-ci-logs/logs/job/1/build-log.txt"},
		{"ObjectBaseURL", b.ObjectBaseURL("job/1"),
			"https://storage.googleapis.com/kubernetes-ci-logs/logs/job/1/"},
		{"ObjectBaseURL trailing slash preserved", b.ObjectBaseURL("job/1/"),
			"https://storage.googleapis.com/kubernetes-ci-logs/logs/job/1/"},
		{"ObjectBaseURL empty", b.ObjectBaseURL(""),
			"https://storage.googleapis.com/kubernetes-ci-logs/logs/"},
		{"WebURL", b.WebURL("job/1/"),
			"https://gcsweb.k8s.io/gcs/kubernetes-ci-logs/logs/job/1/"},
		{"ProwURL", b.ProwURL("job/1"),
			"https://prow.k8s.io/view/gs/kubernetes-ci-logs/logs/job/1"},
		{"ProwURL empty (prefix)", b.ProwURL(""),
			"https://prow.k8s.io/view/gs/kubernetes-ci-logs/logs/"},
		{"ListAPIURL", b.ListAPIURL(),
			"https://storage.googleapis.com/storage/v1/b/kubernetes-ci-logs/o"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestBucketWithDifferentName(t *testing.T) {
	b := NewBucket("my-bucket")
	if got := b.ObjectURL("x"); got != "https://storage.googleapis.com/my-bucket/logs/x" {
		t.Errorf("ObjectURL with custom bucket: %q", got)
	}
}

// TestBuildURLs_Periodic exercises the new location-aware helpers for the
// periodic case. The empty-JobType case asserts the default-to-periodic
// behavior used by legacy ProwJob cache entries.
func TestBuildURLs_Periodic(t *testing.T) {
	b := NewBucket("kubernetes-ci-logs")
	loc := BuildLocation{
		JobLocation: JobLocation{JobType: models.JobTypePeriodic},
		JobName:     "periodic-foo",
		BuildID:     "12345",
	}
	cases := []struct {
		name, got, want string
	}{
		{"BuildObjectURL", b.BuildObjectURL(loc, "build-log.txt"),
			"https://storage.googleapis.com/kubernetes-ci-logs/logs/periodic-foo/12345/build-log.txt"},
		{"BuildBaseURL", b.BuildBaseURL(loc),
			"https://storage.googleapis.com/kubernetes-ci-logs/logs/periodic-foo/12345/"},
		{"BuildWebURL", b.BuildWebURL(loc),
			"https://gcsweb.k8s.io/gcs/kubernetes-ci-logs/logs/periodic-foo/12345/"},
		{"BuildProwURL", b.BuildProwURL(loc),
			"https://prow.k8s.io/view/gs/kubernetes-ci-logs/logs/periodic-foo/12345/"},
		{"JobListPrefix", b.JobListPrefix(loc.JobLocation, loc.JobName, ""),
			"logs/periodic-foo/"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}

	// Empty JobType defaults to periodic so legacy callers stay working.
	empty := BuildLocation{JobName: "j", BuildID: "1"}
	if got := b.BuildObjectURL(empty, "x"); got != "https://storage.googleapis.com/kubernetes-ci-logs/logs/j/1/x" {
		t.Errorf("empty-JobType default: got %q", got)
	}
}

// TestBuildURLs_Presubmit covers the pr-logs layout: the slash in Repo is
// normalized to an underscore to match Prow's path convention.
func TestBuildURLs_Presubmit(t *testing.T) {
	b := NewBucket("kubernetes-ci-logs")
	loc := BuildLocation{
		JobLocation: JobLocation{
			JobType: models.JobTypePresubmit,
			Repo:    "kubernetes-sigs/cluster-api-provider-azure",
		},
		JobName:    "pull-cluster-api-provider-azure-e2e",
		BuildID:    "98765",
		PullNumber: "4321",
	}
	want := func(suffix string) string {
		return "https://storage.googleapis.com/kubernetes-ci-logs/pr-logs/pull/" +
			"kubernetes-sigs_cluster-api-provider-azure/4321/pull-cluster-api-provider-azure-e2e/98765/" + suffix
	}
	cases := []struct {
		name, got, want string
	}{
		{"BuildObjectURL", b.BuildObjectURL(loc, "build-log.txt"), want("build-log.txt")},
		{"BuildBaseURL", b.BuildBaseURL(loc), want("")},
		{"BuildWebURL", b.BuildWebURL(loc),
			"https://gcsweb.k8s.io/gcs/kubernetes-ci-logs/pr-logs/pull/kubernetes-sigs_cluster-api-provider-azure/4321/pull-cluster-api-provider-azure-e2e/98765/"},
		{"BuildProwURL", b.BuildProwURL(loc),
			"https://prow.k8s.io/view/gs/kubernetes-ci-logs/pr-logs/pull/kubernetes-sigs_cluster-api-provider-azure/4321/pull-cluster-api-provider-azure-e2e/98765/"},
		{"JobListPrefix", b.JobListPrefix(loc.JobLocation, loc.JobName, loc.PullNumber),
			"pr-logs/pull/kubernetes-sigs_cluster-api-provider-azure/4321/pull-cluster-api-provider-azure-e2e/"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s:\n got  %q\n want %q", c.name, c.got, c.want)
		}
	}
}

// TestBuildURLs_PresubmitValidation confirms that presubmit URL helpers
// panic when required fields (Repo, PullNumber) are missing, because these
// are construction-time programming errors rather than runtime data.
func TestBuildURLs_PresubmitValidation(t *testing.T) {
	b := NewBucket("kubernetes-ci-logs")
	cases := []struct {
		name string
		loc  BuildLocation
	}{
		{
			name: "missing Repo",
			loc: BuildLocation{
				JobLocation: JobLocation{JobType: models.JobTypePresubmit},
				JobName:     "j", BuildID: "1", PullNumber: "2",
			},
		},
		{
			name: "missing PullNumber",
			loc: BuildLocation{
				JobLocation: JobLocation{JobType: models.JobTypePresubmit, Repo: "org/repo"},
				JobName:     "j", BuildID: "1",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic, got none")
				}
			}()
			_ = b.BuildObjectURL(c.loc, "x")
		})
	}
}
