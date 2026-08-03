// Package actionverify checks proposed remediations against pinned source.
package actionverify

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	StateUnresolved     = "unresolved"
	StateAlreadyPresent = "already_present"
	StateInconclusive   = "inconclusive"
)

type Reader interface {
	ListTree(context.Context) ([]string, error)
	ReadFile(context.Context, string) (string, bool, error)
}

type Input struct {
	Proposal      string
	RelevantFiles []string
}

type Result struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

var implementationPattern = regexp.MustCompile(`(?i)\b(?:implement(?:ing)?|add(?:ing)?|create|define|introduce)\s+(?:the\s+)?\x60?([A-Za-z_][A-Za-z0-9_]{3,})\x60?`)
var pathPattern = regexp.MustCompile(`\x60([^\x60\n]+\.[A-Za-z0-9]{1,8})\x60`)

func Verify(ctx context.Context, reader Reader, input Input) (Result, error) {
	if reader == nil {
		return Result{State: StateInconclusive, Reason: "source reader is unavailable"}, nil
	}
	matches := implementationPattern.FindAllStringSubmatch(input.Proposal, -1)
	if len(matches) == 0 {
		return Result{State: StateInconclusive, Reason: "proposal does not name an implementation symbol"}, nil
	}
	symbols := map[string]bool{}
	for _, match := range matches {
		symbols[match[1]] = true
	}
	paths := append([]string(nil), input.RelevantFiles...)
	for _, match := range pathPattern.FindAllStringSubmatch(input.Proposal, -1) {
		paths = append(paths, match[1])
	}
	paths = compact(paths)
	if len(paths) == 0 {
		return Result{State: StateInconclusive, Reason: "proposal has no grounded source paths"}, nil
	}

	definitions := map[string]bool{}
	calls := map[string]bool{}
	read := 0
	for _, path := range paths {
		content, found, err := reader.ReadFile(ctx, path)
		if err != nil {
			return Result{}, fmt.Errorf("read pinned source %s: %w", path, err)
		}
		if !found {
			continue
		}
		read++
		for symbol := range symbols {
			definition := regexp.MustCompile(`\bfunc\s+(?:\([^)]*\)\s*)?` + regexp.QuoteMeta(symbol) + `\s*\(`)
			call := regexp.MustCompile(`\b` + regexp.QuoteMeta(symbol) + `\s*\(`)
			if definition.MatchString(content) {
				definitions[symbol] = true
			}
			if call.MatchString(content) && !onlyDefinition(content, definition, call) {
				calls[symbol] = true
			}
		}
	}
	if read == 0 {
		return Result{State: StateInconclusive, Reason: "none of the grounded source paths could be read"}, nil
	}
	for symbol := range symbols {
		if definitions[symbol] && calls[symbol] {
			return Result{State: StateAlreadyPresent, Reason: fmt.Sprintf("%s is already defined and invoked at the grounded commit", symbol)}, nil
		}
	}
	return Result{State: StateUnresolved, Reason: "the proposed implementation is not already defined and invoked in the grounded source"}, nil
}

func onlyDefinition(content string, definition, call *regexp.Regexp) bool {
	defs := definition.FindAllStringIndex(content, -1)
	calls := call.FindAllStringIndex(content, -1)
	return len(calls) <= len(defs)
}

func compact(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
