package gcs

import "testing"

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
