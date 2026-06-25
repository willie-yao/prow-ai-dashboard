package prowbuild

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

// startedJSON mirrors the schema of a Prow build's started.json.
type startedJSON struct {
	Timestamp  int64             `json:"timestamp"`
	Repos      map[string]string `json:"repos"`
	RepoCommit string            `json:"repo-commit"`
	RepoVer    string            `json:"repo-version"`
}

// finishedJSON mirrors the schema of a Prow build's finished.json.
type finishedJSON struct {
	Timestamp int64  `json:"timestamp"`
	Passed    bool   `json:"passed"`
	Result    string `json:"result"`
	Revision  string `json:"revision"`
}

// FetchBuildInfo reads started.json and finished.json for the build at loc and
// returns a populated BuildInfo. If finished.json is missing or unreadable the
// build is treated as still running: partial info is returned with
// Result="PENDING" and a zero Finished time.
func FetchBuildInfo(ctx context.Context, b storage.Backend, loc BuildLocation) (*models.BuildInfo, error) {
	buildPath := loc.BuildPath()

	startedData, err := storage.ReadAll(ctx, b, buildPath+"started.json")
	if err != nil {
		return nil, fmt.Errorf("fetching started.json: %w", err)
	}
	var s startedJSON
	if err := json.Unmarshal(startedData, &s); err != nil {
		return nil, fmt.Errorf("parsing started.json: %w", err)
	}

	info := &models.BuildInfo{
		BuildID:     loc.BuildID,
		JobName:     loc.JobName,
		PullNumber:  loc.PullNumber,
		WebURL:      b.WebURL(buildPath),
		ProwURL:     b.ProwURL(buildPath),
		BuildLogURL: b.WebURL(buildPath + "build-log.txt"),
		Started:     time.Unix(s.Timestamp, 0).UTC(),
		Commit:      s.RepoCommit,
		RepoVersion: s.RepoVer,
	}

	// finished.json is absent while the build is still running.
	finishedData, err := storage.ReadAll(ctx, b, buildPath+"finished.json")
	if err != nil {
		info.Result = "PENDING"
		return info, nil
	}
	var f finishedJSON
	if err := json.Unmarshal(finishedData, &f); err != nil {
		return nil, fmt.Errorf("parsing finished.json: %w", err)
	}
	info.Finished = time.Unix(f.Timestamp, 0).UTC()
	info.Passed = f.Passed
	info.Result = f.Result
	info.DurationSeconds = float64(f.Timestamp - s.Timestamp)
	return info, nil
}

// junitFileRe matches JUnit XML filenames produced by the test frameworks we
// have seen: ginkgo (junit.e2e_suite.1.xml), ginkgo's runner report
// (junit_runner.xml), gotest/k8s sharded outputs (junit_01.xml), and a plain
// junit.xml. Anchored against the basename of a build's artifacts/ children.
var junitFileRe = regexp.MustCompile(`^junit[._-].*\.xml$|^junit\.xml$`)

// DiscoverJUnitPaths lists the build's artifacts/ directory and returns the
// bucket-relative path of every JUnit XML file, sorted for cache stability.
// Sub-trees under artifacts/ are not walked. Returns ([], nil) when the
// directory exists but holds no JUnit files.
func DiscoverJUnitPaths(ctx context.Context, b storage.Backend, loc BuildLocation) ([]string, error) {
	artifactsDir := loc.BuildPath() + "artifacts/"
	listing, err := b.List(ctx, artifactsDir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, f := range listing.Files {
		if junitFileRe.MatchString(path.Base(f.Name)) {
			paths = append(paths, artifactsDir+f.Name)
		}
	}
	sort.Strings(paths)
	return paths, nil
}
