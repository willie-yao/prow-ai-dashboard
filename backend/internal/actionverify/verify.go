// Package actionverify checks proposed remediations against pinned source.
package actionverify

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	pathpkg "path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	StateUnresolved     = "unresolved"
	StateAlreadyPresent = "already_present"
	StateInconclusive   = "inconclusive"

	maxExhaustiveGoFiles = 1000
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

type sourceEvidence struct {
	definitions        map[string]map[string]bool
	calls              map[string]map[string]bool
	ambiguousSelectors map[string]bool
}

var implementationPattern = regexp.MustCompile(`(?i)\b(?:implement(?:ing)?\s+(?:the\s+)?|add(?:ing)?\s+(?:a\s+)?(?:call\s+to\s+)?|create\s+|define\s+|introduce\s+)\x60?([A-Za-z_][A-Za-z0-9_]{3,})\x60?`)
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

	evidence := sourceEvidence{
		definitions:        map[string]map[string]bool{},
		calls:              map[string]map[string]bool{},
		ambiguousSelectors: map[string]bool{},
	}
	readPaths := map[string]bool{}
	groundedGoFiles := 0
	for _, path := range paths {
		content, found, err := reader.ReadFile(ctx, path)
		if err != nil {
			return Result{}, fmt.Errorf("read pinned source %s: %w", path, err)
		}
		if !found {
			return Result{State: StateInconclusive, Reason: fmt.Sprintf("grounded source path %s was not found", path)}, nil
		}
		readPaths[path] = true
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		groundedGoFiles++
		if err := inspectGoSource(path, content, symbols, &evidence); err != nil {
			return Result{State: StateInconclusive, Reason: "grounded Go source could not be parsed"}, nil
		}
	}
	if groundedGoFiles == 0 {
		return Result{State: StateInconclusive, Reason: "none of the grounded source paths are Go source"}, nil
	}
	if allSymbolsMatched(symbols, evidence) {
		return alreadyPresentResult(symbols), nil
	}

	tree, err := reader.ListTree(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("list pinned source tree: %w", err)
	}
	goPaths := make([]string, 0, len(tree))
	for _, path := range compact(tree) {
		if strings.HasSuffix(path, ".go") {
			goPaths = append(goPaths, path)
		}
	}
	if len(goPaths) == 0 {
		return Result{State: StateInconclusive, Reason: "pinned source tree contains no Go source"}, nil
	}
	if len(goPaths) > maxExhaustiveGoFiles {
		return Result{State: StateInconclusive, Reason: "pinned source tree is too large for exhaustive verification"}, nil
	}
	for _, path := range goPaths {
		if readPaths[path] {
			continue
		}
		content, found, err := reader.ReadFile(ctx, path)
		if err != nil {
			return Result{}, fmt.Errorf("read pinned source %s: %w", path, err)
		}
		if !found {
			return Result{State: StateInconclusive, Reason: fmt.Sprintf("pinned source path %s was not found", path)}, nil
		}
		if err := inspectGoSource(path, content, symbols, &evidence); err != nil {
			return Result{State: StateInconclusive, Reason: "pinned Go source could not be parsed exhaustively"}, nil
		}
	}
	if allSymbolsMatched(symbols, evidence) {
		return alreadyPresentResult(symbols), nil
	}
	for symbol := range symbols {
		if symbolMatched(symbol, evidence) {
			continue
		}
		if evidence.ambiguousSelectors[symbol] || packageIdentityAmbiguous(symbol, evidence) {
			return Result{State: StateInconclusive, Reason: fmt.Sprintf("selector identity for %s could not be resolved", symbol)}, nil
		}
	}
	return Result{State: StateUnresolved, Reason: "the proposed implementation is not already defined and invoked in the pinned source"}, nil
}

func inspectGoSource(path, content string, symbols map[string]bool, evidence *sourceEvidence) error {
	file, err := parser.ParseFile(token.NewFileSet(), path, content, 0)
	if err != nil {
		return err
	}
	packageName := file.Name.Name
	imports := map[string]string{}
	hasDotImport := false
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		alias := pathpkg.Base(importPath)
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		switch alias {
		case ".":
			hasDotImport = true
		case "", "_":
		default:
			imports[alias] = pathpkg.Base(importPath)
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.FuncDecl:
			if symbols[value.Name.Name] {
				markPackage(evidence.definitions, value.Name.Name, packageName)
			}
		case *ast.CallExpr:
			name, callPackage, ambiguous := "", "", false
			switch fun := value.Fun.(type) {
			case *ast.Ident:
				name = fun.Name
				switch {
				case fun.Obj != nil && fun.Obj.Kind != ast.Fun:
					ambiguous = true
				case fun.Obj == nil && hasDotImport:
					ambiguous = true
				default:
					callPackage = packageName
				}
			case *ast.SelectorExpr:
				name = fun.Sel.Name
				qualifier, ok := fun.X.(*ast.Ident)
				if ok && qualifier.Obj == nil {
					callPackage = imports[qualifier.Name]
				}
				ambiguous = callPackage == ""
			}
			if !symbols[name] {
				break
			}
			if ambiguous {
				evidence.ambiguousSelectors[name] = true
			} else if callPackage != "" {
				markPackage(evidence.calls, name, callPackage)
			}
		}
		return true
	})
	return nil
}

func allSymbolsMatched(symbols map[string]bool, evidence sourceEvidence) bool {
	for symbol := range symbols {
		if !symbolMatched(symbol, evidence) {
			return false
		}
	}
	return true
}

func symbolMatched(symbol string, evidence sourceEvidence) bool {
	for packageName := range evidence.definitions[symbol] {
		if evidence.calls[symbol][packageName] {
			return true
		}
	}
	return false
}

func packageIdentityAmbiguous(symbol string, evidence sourceEvidence) bool {
	if len(evidence.definitions[symbol]) == 0 || len(evidence.calls[symbol]) == 0 {
		return false
	}
	return !symbolMatched(symbol, evidence)
}

func alreadyPresentResult(symbols map[string]bool) Result {
	names := make([]string, 0, len(symbols))
	for symbol := range symbols {
		names = append(names, symbol)
	}
	sort.Strings(names)
	return Result{State: StateAlreadyPresent, Reason: fmt.Sprintf("%s are already defined and invoked at the grounded commit", strings.Join(names, ", "))}
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
