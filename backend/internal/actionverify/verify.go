// Package actionverify checks proposed remediations against pinned source.
package actionverify

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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
	definitions := map[string]map[string]bool{}
	calls := map[string]map[string]bool{}
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
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, content, 0)
		if err != nil {
			return Result{State: StateInconclusive, Reason: "grounded Go source could not be parsed"}, nil
		}
		packageName := file.Name.Name
		ast.Inspect(file, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.FuncDecl:
				if symbols[value.Name.Name] {
					markPackage(definitions, value.Name.Name, packageName)
				}
			case *ast.CallExpr:
				name, callPackage := "", packageName
				switch fun := value.Fun.(type) {
				case *ast.Ident:
					name = fun.Name
				case *ast.SelectorExpr:
					name = fun.Sel.Name
					if qualifier, ok := fun.X.(*ast.Ident); ok {
						callPackage = qualifier.Name
					} else {
						callPackage = ""
					}
				}
				if symbols[name] && callPackage != "" {
					markPackage(calls, name, callPackage)
				}
			}
			return true
		})
	}
	if read == 0 {
		return Result{State: StateInconclusive, Reason: "none of the grounded source paths could be read"}, nil
	}
	for symbol := range symbols {
		matched := false
		for packageName := range definitions[symbol] {
			if calls[symbol][packageName] {
				matched = true
				break
			}
		}
		if !matched {
			return Result{State: StateUnresolved, Reason: "the proposed implementation is not already defined and invoked in the grounded source"}, nil
		}
	}
	names := make([]string, 0, len(symbols))
	for symbol := range symbols {
		names = append(names, symbol)
	}
	sort.Strings(names)
	return Result{State: StateAlreadyPresent, Reason: fmt.Sprintf("%s are already defined and invoked at the grounded commit", strings.Join(names, ", "))}, nil
}
func markPackage(values map[string]map[string]bool, symbol, packageName string) {
	if values[symbol] == nil {
		values[symbol] = map[string]bool{}
	}
	values[symbol][packageName] = true
}

func compact(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
