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
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return fmt.Errorf("prompt author: generated prompt is empty")
	}
	if len(prompt) > maxBytes {
		return fmt.Errorf("prompt author: generated prompt exceeds %d bytes", maxBytes)
	}
	var headings []string
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			headings = append(headings, line)
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
	for _, heading := range []string{"## Architecture", "## Diagnostic lifecycle"} {
		if sectionIsEmpty(prompt, heading) {
			return fmt.Errorf("prompt author: %s is empty", heading)
		}
	}
	operational := 0
	for _, heading := range []string{"## Artifact layout", "## Common failure patterns", "## Triage order"} {
		if !sectionIsEmpty(prompt, heading) {
			operational++
		}
	}
	if operational == 0 {
		return fmt.Errorf("prompt author: generated prompt has no operational evidence section")
	}
	return nil
}

func sectionIsEmpty(prompt, heading string) bool {
	start := strings.Index(prompt, heading)
	if start < 0 {
		return true
	}
	body := prompt[start+len(heading):]
	if next := strings.Index(body, "\n## "); next >= 0 {
		body = body[:next]
	}
	body = strings.TrimSpace(body)
	return body == "" || strings.Contains(strings.ToLower(body), "todo:") && len(strings.Fields(body)) < 12
}
