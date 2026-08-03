// Package actiondraft validates model-generated issue and pull request text
// before it can enter an operator preview.
package actiondraft

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxTitleRunes = 256
	MaxTitleBytes = 1024
	MaxBodyBytes  = 64 << 10
)

var headingPattern = regexp.MustCompile(`^#{1,3}[ \t]+(.+?)[ \t]*#*[ \t]*$`)
var numberedDraftPattern = regexp.MustCompile(`(?im)^[ \t]*(?:draft|version|option)[ \t]+[12][:.][ \t]*`)

var rejectedPhrases = []string{
	"the user wants me",
	"i need to",
	"let me draft",
	"current structure",
	"this looks good",
	"one final check",
	"maintainer instruction:",
	"current issue body:",
	"description to fit into the template:",
	"candidate templates:",
	"report content:",
	"pull request template:",
	"system prompt:",
	"developer instruction:",
	"final answer:",
	"revised issue body:",
	"<!-- prow-ai-dashboard-key:",
}

// ValidateBody rejects unsafe or malformed model-generated draft text.
func ValidateBody(body string) error {
	if err := validateText("body", body, MaxBodyBytes); err != nil {
		return err
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("body is empty")
	}
	if err := validateNoRejectedPhrases("body", body); err != nil {
		return err
	}
	if len(numberedDraftPattern.FindAllStringIndex(body, -1)) > 1 {
		return fmt.Errorf("body contains multiple draft variants")
	}
	if err := validateUniqueSections(body); err != nil {
		return err
	}
	return nil
}

// ValidateTitleBody validates a complete issue draft.
func ValidateTitleBody(title, body string) error {
	if err := validateText("title", title, MaxTitleBytes); err != nil {
		return err
	}
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("title is empty")
	}
	if utf8.RuneCountInString(title) > MaxTitleRunes {
		return fmt.Errorf("title exceeds %d characters", MaxTitleRunes)
	}
	if err := validateNoRejectedPhrases("title", title); err != nil {
		return err
	}
	return ValidateBody(body)
}

func validateNoRejectedPhrases(field, value string) error {
	lower := strings.ToLower(value)
	for _, phrase := range rejectedPhrases {
		if strings.Contains(lower, phrase) {
			return fmt.Errorf("%s contains model reasoning or prompt text", field)
		}
	}
	return nil
}

func validateText(field, value string, maxBytes int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", field)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s exceeds %d bytes", field, maxBytes)
	}
	for _, r := range value {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return fmt.Errorf("%s contains control characters", field)
		}
	}
	return nil
}

func validateUniqueSections(body string) error {
	seenSections := map[string]bool{}
	affectedBuilds := false
	seenBuilds := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if match := headingPattern.FindStringSubmatch(line); match != nil {
			name := normalizeHeading(match[1])
			if name != "" && seenSections[name] {
				return fmt.Errorf("body repeats issue section %q", name)
			}
			if name != "" {
				seenSections[name] = true
			}
			affectedBuilds = name == "affected builds"
			continue
		}
		if !affectedBuilds {
			continue
		}
		entry := strings.TrimSpace(line)
		if !strings.HasPrefix(entry, "- ") && !strings.HasPrefix(entry, "* ") {
			continue
		}
		entry = strings.Join(strings.Fields(strings.ToLower(entry[2:])), " ")
		if entry != "" && seenBuilds[entry] {
			return fmt.Errorf("body repeats an affected build entry")
		}
		seenBuilds[entry] = entry != ""
	}
	return nil
}

func normalizeHeading(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "*_`~ ")
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}
