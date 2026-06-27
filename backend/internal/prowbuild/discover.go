package prowbuild

import (
	"context"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

// DiscoverJobs finds a project's jobs by listing the artifact bucket's own Prow
// indexes, with no dependency on a job-config repo. It is the project-agnostic
// alternative to testgrid-based discovery and suits a bucket dedicated to one
// project.
//
//   - Periodics and postsubmits come from job-name subdirectories of logs/.
//   - Presubmits, when includePresubmits is set, come from the job-name
//     subdirectories of pr-logs/directory/; each job's repo is resolved by
//     reading one of its index entries.
//
// jobFilters, when non-empty, keeps only job names containing one of the
// substrings case-insensitively. Omit to take every job in the bucket.
func DiscoverJobs(ctx context.Context, b storage.Backend, includePresubmits bool, jobFilters []string) ([]models.ProwJob, error) {
	var jobs []models.ProwJob

	periodics, err := b.List(ctx, "logs/")
	if err != nil {
		return nil, err
	}
	for _, d := range periodics.Dirs {
		name := strings.TrimSuffix(d, "/")
		if name == "" || !matchesFilters(name, jobFilters) {
			continue
		}
		jobs = append(jobs, models.ProwJob{
			Name:    name,
			TabName: name,
			JobType: models.JobTypePeriodic,
			JobID:   models.JobIDFor(models.JobTypePeriodic, "", name),
		})
	}

	if includePresubmits {
		presubmits, err := b.List(ctx, "pr-logs/directory/")
		if err != nil {
			return nil, err
		}
		for _, d := range presubmits.Dirs {
			name := strings.TrimSuffix(d, "/")
			if name == "" || !matchesFilters(name, jobFilters) {
				continue
			}
			repo, ok := resolvePresubmitRepo(ctx, b, name)
			if !ok {
				continue // no resolvable builds; skip
			}
			jobs = append(jobs, models.ProwJob{
				Name:    name,
				TabName: name,
				JobType: models.JobTypePresubmit,
				Repo:    repo,
				JobID:   models.JobIDFor(models.JobTypePresubmit, repo, name),
			})
		}
	}

	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Name < jobs[j].Name })
	return jobs, nil
}

// matchesFilters reports whether name contains any filter substring.
// An empty filter list matches everything.
func matchesFilters(name string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	lower := strings.ToLower(name)
	for _, f := range filters {
		if strings.Contains(lower, strings.ToLower(f)) {
			return true
		}
	}
	return false
}

// resolvePresubmitRepo reads the newest index entry under
// pr-logs/directory/<job>/ and returns the job's "org/repo". The bucket index
// only encodes "<org>_<repo>" in the entry body, so the first underscore is
// treated as the org/repo separator.
func resolvePresubmitRepo(ctx context.Context, b storage.Backend, jobName string) (string, bool) {
	listing, err := b.List(ctx, "pr-logs/directory/"+jobName+"/")
	if err != nil {
		return "", false
	}
	var ids []string
	for _, f := range listing.Files {
		base := strings.TrimSuffix(f.Name, ".txt")
		if base != f.Name && isNumeric(base) {
			ids = append(ids, base)
		}
	}
	if len(ids) == 0 {
		return "", false
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	body, err := storage.ReadAll(ctx, b, "pr-logs/directory/"+jobName+"/"+ids[0]+".txt")
	if err != nil {
		return "", false
	}
	parts, ok := splitPresubmitRef(string(body))
	if !ok {
		return "", false
	}
	return strings.Replace(parts[2], "_", "/", 1), true
}
