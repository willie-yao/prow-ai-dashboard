package remediation

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aggregator"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/junit"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/resolve"
)

// EvidenceForPattern captures deterministic failures from the pattern's builds.
func EvidenceForPattern(pattern models.PatternAnalysis, details []models.JobDetail) Evidence {
	buildSet := make(map[string]struct{}, len(pattern.SharedBuilds))
	for _, buildID := range pattern.SharedBuilds {
		buildSet[buildID] = struct{}{}
	}
	tests := map[string]*TestEvidence{}
	for _, detail := range details {
		if detail.JobID != pattern.JobID && detail.Name != pattern.Subject {
			continue
		}
		for _, run := range detail.Runs {
			if len(buildSet) > 0 {
				if _, ok := buildSet[run.BuildID]; !ok {
					continue
				}
			}
			for _, test := range run.TestCases {
				if test.Status != "failed" {
					continue
				}
				identity := junit.Identity(test)
				entry := tests[identity]
				if entry == nil {
					entry = &TestEvidence{
						Identity: identity, Name: test.Name, SuiteName: test.SuiteName,
						ClassName: test.ClassName,
					}
					tests[identity] = entry
				}
				message := test.FailureMessage + "\n" + test.FailureBody
				hash := aggregator.HashError(aggregator.NormalizeErrorMessage(message))
				if entry.ErrorHash == "" {
					entry.ErrorHash = hash
				}
				entry.BuildIDs = appendUnique(entry.BuildIDs, run.BuildID)
				entry.JUnitFiles = appendUnique(entry.JUnitFiles, test.JUnitFile)
			}
		}
		break
	}
	outTests := make([]TestEvidence, 0, len(tests))
	for _, test := range tests {
		sort.Strings(test.BuildIDs)
		sort.Strings(test.JUnitFiles)
		outTests = append(outTests, *test)
	}
	sort.Slice(outTests, func(i, j int) bool { return outTests[i].Identity < outTests[j].Identity })
	rootCause := strings.TrimSpace(pattern.SharedRootCause)
	sum := sha256.Sum256([]byte(strings.ToLower(rootCause)))
	return Evidence{
		PatternID: pattern.ID, RootCause: rootCause, RootCauseHash: hex.EncodeToString(sum[:8]),
		BuildWatermark: resolve.Watermark(pattern),
		AffectedBuilds: append([]string(nil), pattern.SharedBuilds...), Tests: outTests,
		EvidenceCreated: nowString(),
	}
}

func appendUnique(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func classificationForPattern(pattern models.PatternAnalysis, details []models.JobDetail) string {
	for _, detail := range details {
		if detail.JobID != pattern.JobID && detail.Name != pattern.Subject {
			continue
		}
		for _, test := range EvidenceForPattern(pattern, details).Tests {
			info := aggregator.ClassifyFailure(test.Name, detail.Runs, 3)
			if info.Classification == models.ClassificationFlaky {
				return string(models.ClassificationFlaky)
			}
			if info.Classification == models.ClassificationPersistent {
				return string(models.ClassificationPersistent)
			}
		}
	}
	return "pattern"
}
