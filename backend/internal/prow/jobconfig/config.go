// Package jobconfig parses Prow job configuration YAML files from the
// kubernetes/test-infra repository and extracts job metadata for the
// configured testgrid dashboard.
package jobconfig

import (
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"gopkg.in/yaml.v3"
)

// rawJob is the intermediate representation used to unmarshal a single job
// entry from the Prow YAML before converting it to a models.ProwJob.
type rawJob struct {
	Name             string            `yaml:"name"`
	MinimumInterval  string            `yaml:"minimum_interval"`
	DecorationConfig *decorationConfig `yaml:"decoration_config"`
	ExtraRefs        []extraRef        `yaml:"extra_refs"`
	Annotations      map[string]string `yaml:"annotations"`
}

type decorationConfig struct {
	Timeout string `yaml:"timeout"`
}

type extraRef struct {
	BaseRef string `yaml:"base_ref"`
}

// periodicsFile represents the top-level structure of a periodics YAML file.
type periodicsFile struct {
	Periodics []rawJob `yaml:"periodics"`
}

// presubmitsFile represents the top-level structure of a presubmits YAML file.
type presubmitsFile struct {
	Presubmits map[string][]rawJob `yaml:"presubmits"`
}

// ParseJobConfig parses a Prow YAML config file and returns the jobs whose
// testgrid-dashboards annotation contains the given dashboard. filename is
// recorded in each returned ProwJob's ConfigFile field.
func ParseJobConfig(data []byte, filename, dashboard string) ([]models.ProwJob, error) {
	// Try periodics first.
	var pf periodicsFile
	if err := yaml.Unmarshal(data, &pf); err != nil {
		return nil, err
	}

	var raw []rawJob
	if len(pf.Periodics) > 0 {
		raw = pf.Periodics
	} else {
		// Try presubmits.
		var psf presubmitsFile
		if err := yaml.Unmarshal(data, &psf); err != nil {
			return nil, err
		}
		for _, jobs := range psf.Presubmits {
			raw = append(raw, jobs...)
		}
	}

	var result []models.ProwJob
	for _, r := range raw {
		if !matchesDashboard(r, dashboard) {
			continue
		}
		result = append(result, convertJob(r, filename))
	}
	return result, nil
}

// matchesDashboard returns true when the job's testgrid-dashboards annotation
// contains the given dashboard name.
func matchesDashboard(r rawJob, dashboard string) bool {
	dashboards := r.Annotations["testgrid-dashboards"]
	for _, d := range strings.Split(dashboards, ",") {
		if strings.TrimSpace(d) == dashboard {
			return true
		}
	}
	return false
}

func convertJob(r rawJob, filename string) models.ProwJob {
	j := models.ProwJob{
		Name:            r.Name,
		TabName:         r.Annotations["testgrid-tab-name"],
		Description:     r.Annotations["description"],
		MinimumInterval: r.MinimumInterval,
		ConfigFile:      filename,
		Category:        inferCategory(r.Name),
	}
	if r.DecorationConfig != nil {
		j.Timeout = r.DecorationConfig.Timeout
	}
	if len(r.ExtraRefs) > 0 {
		j.Branch = r.ExtraRefs[0].BaseRef
	}
	return j
}

// inferCategory maps well-known substrings in a job name to a category.
func inferCategory(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "conformance"):
		return "conformance"
	case strings.Contains(lower, "capi-e2e"):
		return "capi-e2e"
	case strings.Contains(lower, "e2e-aks") || strings.Contains(lower, "[managed kubernetes]"):
		return "aks-e2e"
	case strings.Contains(lower, "upgrade"):
		return "upgrade"
	case strings.Contains(lower, "coverage"):
		return "coverage"
	case strings.Contains(lower, "scalability"):
		return "scalability"
	case strings.Contains(lower, "e2e"):
		return "capz-e2e"
	default:
		return "other"
	}
}
