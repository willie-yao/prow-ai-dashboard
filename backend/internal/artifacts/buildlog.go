package artifacts

import (
	"bufio"
	"bytes"
	"regexp"
	"strings"
)

var (
	// Build logs use Unicode curly quotes (\u201c \u201d) around test names.
	infoRe = regexp.MustCompile(`INFO: [\x{201c}"'](.+?)[\x{201d}"'] started at`)
	// STEP lines may contain ANSI escape codes for formatting.
	namespaceRe = regexp.MustCompile(`STEP:.*?Creating namespace "(capz-e2e-[a-z0-9]+)"`)
	// Strip ANSI escape sequences for clean parsing.
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)
)

// ParseNamespaceMap parses a build log to extract the mapping from test name
// fragments to Kubernetes namespace names.
// Returns a map where keys are lowercased test name fragments and values are namespace names.
func ParseNamespaceMap(buildLog []byte) map[string]string {
	result := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(buildLog))

	var currentTestFragment string

	for scanner.Scan() {
		line := ansiRe.ReplaceAllString(scanner.Text(), "")

		if m := infoRe.FindStringSubmatch(line); m != nil {
			currentTestFragment = m[1]
			continue
		}

		if currentTestFragment != "" {
			if m := namespaceRe.FindStringSubmatch(line); m != nil {
				result[strings.ToLower(currentTestFragment)] = m[1]
				currentTestFragment = ""
			}
		}
	}

	return result
}

// FindNamespaceForTest finds the namespace that corresponds to a given JUnit
// test case name by matching against the fragment map from ParseNamespaceMap.
// The match is done by checking if the test name (lowercased) contains the
// fragment (lowercased). Returns "" if no match found.
func FindNamespaceForTest(testName string, namespaceMap map[string]string) string {
	lowerTestName := strings.ToLower(testName)
	for fragment, ns := range namespaceMap {
		if strings.Contains(lowerTestName, fragment) {
			return ns
		}
	}
	return ""
}
