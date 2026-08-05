package promptauthor

import (
	"fmt"
	"strings"
)

var requiredHeadings = []string{
	"## Architecture",
	"## Diagnostic lifecycle",
	"## Test and job flavors",
	"## Artifact layout",
	"## Common failure patterns",
	"## Transient classification",
	"## Triage order",
	"## Relevant source repositories",
	"## Unresolved details",
}

// Validate enforces the deterministic prompt-author output contract.
func Validate(prompt string) error {
	if len(prompt) > maxBytes {
		return fmt.Errorf("prompt author: generated prompt exceeds %d bytes", maxBytes)
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return fmt.Errorf("prompt author: generated prompt is empty")
	}
	lines := strings.Split(prompt, "\n")
	var headings []string
	var headingLines []int
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			headings = append(headings, line)
			headingLines = append(headingLines, i)
		}
	}
	if len(headings) != len(requiredHeadings) {
		return fmt.Errorf("prompt author: generated prompt has %d level-two sections, want %d", len(headings), len(requiredHeadings))
	}
	for i, heading := range requiredHeadings {
		if headings[i] != heading {
			return fmt.Errorf("prompt author: section %d is %q, want %q", i+1, headings[i], heading)
		}
	}
	for _, index := range []int{0, 1} {
		if sectionLinesEmpty(lines, headingLines, index) {
			return fmt.Errorf("prompt author: %s is empty", requiredHeadings[index])
		}
	}
	operational := 0
	for _, index := range []int{3, 4, 6} {
		if !sectionLinesEmpty(lines, headingLines, index) {
			operational++
		}
	}
	if operational == 0 {
		return fmt.Errorf("prompt author: generated prompt has no operational evidence section")
	}
	return nil
}

func sectionLinesEmpty(lines []string, headingLines []int, index int) bool {
	start := headingLines[index] + 1
	end := len(lines)
	if index+1 < len(headingLines) {
		end = headingLines[index+1]
	}
	body := strings.TrimSpace(strings.Join(lines[start:end], "\n"))
	return body == "" || strings.Contains(strings.ToLower(body), "todo:") && len(strings.Fields(body)) < 12
}
