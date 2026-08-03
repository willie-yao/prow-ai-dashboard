// Package actionverify checks proposed remediations against pinned source.
package actionverify

import (
	"bufio"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	goscanner "go/scanner"
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

type bulkReader interface {
	ReadFiles(context.Context, []string) (map[string]string, error)
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

type moduleRoot struct {
	dir  string
	path string
}

type packageResolver struct {
	modules      []moduleRoot
	packageNames map[string]string
}

var implementationVerbPattern = regexp.MustCompile(`(?i)\b(?:implement(?:ing)?|add(?:ing)?|create|define|introduce|call(?:ing)?|invoke|invoking)\b`)
var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var identifierTokenPattern = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{3,}`)
var quotedCodePattern = regexp.MustCompile(`^((?:[A-Za-z_][A-Za-z0-9_]*\.)*[A-Za-z_][A-Za-z0-9_]*)(?:\(\))?$`)
var backtickSpanPattern = regexp.MustCompile(`\x60[^\x60\n]+\x60`)
var pathPattern = regexp.MustCompile(`\x60([^\x60\n]+\.[A-Za-z0-9]{1,8})\x60`)
var sourceExtensions = map[string]bool{
	".bash": true, ".c": true, ".cc": true, ".cfg": true, ".conf": true, ".cpp": true,
	".css": true, ".go": true, ".h": true, ".hpp": true, ".html": true, ".ini": true,
	".java": true, ".js": true, ".json": true, ".jsx": true, ".kt": true, ".kts": true,
	".md": true, ".mod": true, ".proto": true, ".py": true, ".rb": true, ".rs": true,
	".scss": true, ".sh": true, ".sql": true, ".sum": true, ".toml": true, ".tpl": true,
	".tmpl": true, ".ts": true, ".tsx": true, ".txt": true, ".xml": true, ".yaml": true,
	".yml": true, ".zsh": true,
}
var constrainedGOOS = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true,
	"hurd": true, "illumos": true, "ios": true, "js": true, "linux": true, "netbsd": true,
	"openbsd": true, "plan9": true, "solaris": true, "wasip1": true, "windows": true, "zos": true,
}
var constrainedGOARCH = map[string]bool{
	"386": true, "amd64": true, "arm": true, "arm64": true, "loong64": true, "mips": true,
	"mips64": true, "mips64le": true, "mipsle": true, "ppc64": true, "ppc64le": true,
	"riscv64": true, "s390x": true, "sparc64": true, "wasm": true,
}

func Verify(ctx context.Context, reader Reader, input Input) (Result, error) {
	if reader == nil {
		return Result{State: StateInconclusive, Reason: "source reader is unavailable"}, nil
	}
	symbols, ambiguousSymbols := implementationSymbols(input.Proposal)
	if len(symbols) == 0 {
		return Result{State: StateInconclusive, Reason: "proposal does not name an unambiguous implementation symbol"}, nil
	}
	if ambiguousSymbols {
		return Result{State: StateInconclusive, Reason: "proposal contains ambiguous implementation symbols"}, nil
	}
	groundedPaths := append([]string(nil), input.RelevantFiles...)
	for _, match := range pathPattern.FindAllStringSubmatch(input.Proposal, -1) {
		if proposalSourcePath(match[1]) {
			groundedPaths = append(groundedPaths, match[1])
		}
	}
	groundedPaths = compact(groundedPaths)
	if len(groundedPaths) == 0 {
		return Result{State: StateInconclusive, Reason: "proposal has no grounded source paths"}, nil
	}

	tree, err := reader.ListTree(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("list pinned source tree: %w", err)
	}
	tree = compact(tree)
	treeSet := make(map[string]bool, len(tree))
	goPaths := make([]string, 0, len(tree))
	modulePaths := make([]string, 0)
	for _, path := range tree {
		treeSet[path] = true
		if strings.HasSuffix(path, ".go") {
			goPaths = append(goPaths, path)
		}
		if pathpkg.Base(path) == "go.mod" {
			modulePaths = append(modulePaths, path)
		}
	}
	groundedGoFiles := 0
	for _, path := range groundedPaths {
		if !treeSet[path] {
			return Result{State: StateInconclusive, Reason: fmt.Sprintf("grounded source path %s was not found", path)}, nil
		}
		if strings.HasSuffix(path, ".go") {
			groundedGoFiles++
		}
	}
	if groundedGoFiles == 0 {
		return Result{State: StateInconclusive, Reason: "none of the grounded source paths are Go source"}, nil
	}
	if len(goPaths) == 0 {
		return Result{State: StateInconclusive, Reason: "pinned source tree contains no Go source"}, nil
	}
	if len(goPaths) > maxExhaustiveGoFiles {
		return Result{State: StateInconclusive, Reason: "pinned source tree is too large for exhaustive verification"}, nil
	}

	readPaths := compact(append(append([]string(nil), goPaths...), modulePaths...))
	contents, err := readSourceFiles(ctx, reader, readPaths)
	if err != nil {
		return Result{}, err
	}
	for _, path := range readPaths {
		if _, ok := contents[path]; !ok {
			return Result{State: StateInconclusive, Reason: fmt.Sprintf("pinned source path %s was not found", path)}, nil
		}
	}
	resolver := newPackageResolver(contents, modulePaths, goPaths)
	evidence := sourceEvidence{
		definitions:        map[string]map[string]bool{},
		calls:              map[string]map[string]bool{},
		ambiguousSelectors: map[string]bool{},
	}
	constrainedSymbols := map[string]bool{}
	groundedTestDirs := map[string]bool{}
	inspected := map[string]bool{}
	anchors := map[string]map[string]bool{}
	for symbol := range symbols {
		anchors[symbol] = map[string]bool{}
	}
	for _, path := range groundedPaths {
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		if strings.HasSuffix(path, "_test.go") {
			groundedTestDirs[pathpkg.Dir(path)] = true
		}
		constrained := isBuildConstrained(path, contents[path])
		target := &evidence
		if constrained {
			target = &sourceEvidence{
				definitions: map[string]map[string]bool{}, calls: map[string]map[string]bool{}, ambiguousSelectors: map[string]bool{},
			}
		}
		packageID, err := inspectGoSource(path, contents[path], symbols, resolver, target)
		if err != nil {
			return Result{State: StateInconclusive, Reason: "grounded Go source could not be parsed"}, nil
		}
		for symbol := range symbols {
			anchors[symbol][packageID] = true
			if constrained && (len(target.definitions[symbol]) > 0 || len(target.calls[symbol]) > 0 || target.ambiguousSelectors[symbol]) {
				constrainedSymbols[symbol] = true
			}
		}
		inspected[path] = true
	}
	for symbol := range symbols {
		for packageID := range evidence.definitions[symbol] {
			anchors[symbol][packageID] = true
		}
		for packageID := range evidence.calls[symbol] {
			anchors[symbol][packageID] = true
		}
	}
	if allSymbolsMatched(symbols, evidence) {
		return alreadyPresentResult(symbols), nil
	}
	for _, path := range goPaths {
		if inspected[path] || strings.HasSuffix(path, "_test.go") && !groundedTestDirs[pathpkg.Dir(path)] ||
			!containsCandidateIdentifier(contents[path], symbols) {
			continue
		}
		extra := sourceEvidence{
			definitions:        map[string]map[string]bool{},
			calls:              map[string]map[string]bool{},
			ambiguousSelectors: map[string]bool{},
		}
		packageID, err := inspectGoSource(path, contents[path], symbols, resolver, &extra)
		if err != nil {
			return Result{State: StateInconclusive, Reason: "pinned Go source could not be parsed exhaustively"}, nil
		}
		if isBuildConstrained(path, contents[path]) {
			for symbol := range symbols {
				for candidatePackage := range extra.definitions[symbol] {
					constrainedSymbols[symbol] = constrainedSymbols[symbol] || anchors[symbol][candidatePackage]
				}
				for candidatePackage := range extra.calls[symbol] {
					constrainedSymbols[symbol] = constrainedSymbols[symbol] || anchors[symbol][candidatePackage]
				}
				if extra.ambiguousSelectors[symbol] && anchors[symbol][packageID] {
					constrainedSymbols[symbol] = true
				}
			}
			continue
		}
		mergeAnchoredEvidence(&evidence, extra, packageID, anchors, symbols)
	}
	if allSymbolsMatched(symbols, evidence) {
		return alreadyPresentResult(symbols), nil
	}
	for symbol := range symbols {
		if symbolMatched(symbol, evidence) {
			continue
		}
		if constrainedSymbols[symbol] {
			return Result{State: StateInconclusive, Reason: fmt.Sprintf("matching evidence for %s is build constrained", symbol)}, nil
		}
		if evidence.ambiguousSelectors[symbol] || packageIdentityAmbiguous(symbol, evidence) {
			return Result{State: StateInconclusive, Reason: fmt.Sprintf("selector identity for %s could not be resolved", symbol)}, nil
		}
	}
	return Result{State: StateUnresolved, Reason: "the proposed implementation is not already defined and invoked in the pinned source"}, nil
}

func isBuildConstrained(filePath, content string) bool {
	base := strings.TrimSuffix(pathpkg.Base(filePath), ".go")
	base = strings.TrimSuffix(base, "_test")
	parts := strings.Split(base, "_")
	if len(parts) > 1 {
		suffix := parts[len(parts)-1]
		if constrainedGOOS[suffix] || constrainedGOARCH[suffix] {
			return true
		}
	}
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "//go:build ") || strings.HasPrefix(line, "// +build ") {
			return true
		}
		if strings.HasPrefix(line, "package ") {
			break
		}
	}
	return false
}

// HasImplementationSymbols reports whether proposal names code-like remediation symbols.
func HasImplementationSymbols(proposal string) bool {
	symbols, _ := implementationSymbols(proposal)
	return len(symbols) > 0
}

func implementationSymbols(proposal string) (map[string]bool, bool) {
	symbols := map[string]bool{}
	ambiguous := false
	proposal = backtickSpanPattern.ReplaceAllStringFunc(proposal, func(span string) string {
		if _, ok := quotedImplementationSymbol(span); ok {
			return span
		}
		return strings.Repeat(" ", len(span))
	})
	for _, location := range implementationVerbPattern.FindAllStringIndex(proposal, -1) {
		end := location[1] + 256
		if end > len(proposal) {
			end = len(proposal)
		}
		clause := proposal[location[1]:end]
		if boundary := implementationClauseBoundary(clause); boundary >= 0 {
			clause = clause[:boundary]
		}
		quoted := backtickSpanPattern.FindAllString(clause, -1)
		for _, span := range quoted {
			if symbol, ok := quotedImplementationSymbol(span); ok {
				symbols[symbol] = true
			}
		}
		unquotedClause := backtickSpanPattern.ReplaceAllString(clause, " ")
		candidates := identifierTokenPattern.FindAllStringIndex(unquotedClause, -1)
		previousEnd := -1
	candidateLoop:
		for _, location := range candidates {
			candidate := unquotedClause[location[0]:location[1]]
			if !codeLikeIdentifier(candidate) {
				continue
			}
			if previousEnd < 0 {
				symbols[candidate] = true
				previousEnd = location[1]
				continue
			}
			gap := strings.ToLower(strings.TrimSpace(unquotedClause[previousEnd:location[0]]))
			switch {
			case gap == "and" || gap == "or" || gap == "," || gap == ", and" || gap == ", or":
				symbols[candidate] = true
				previousEnd = location[1]
			case strings.HasPrefix(gap, "using") || strings.HasPrefix(gap, "with") || strings.HasPrefix(gap, "via") ||
				strings.HasPrefix(gap, "through") || strings.HasPrefix(gap, "for") || strings.HasPrefix(gap, "on") || strings.HasPrefix(gap, "in"):
				break candidateLoop
			case implementationVerbPattern.MatchString(gap):
				previousEnd = location[1]
			default:
				ambiguous = true
				previousEnd = location[1]
			}
		}
	}
	return symbols, ambiguous
}

func implementationClauseBoundary(clause string) int {
	inCode := false
	for i := range len(clause) {
		if clause[i] == '`' {
			inCode = !inCode
			continue
		}
		if !inCode && strings.ContainsRune(".!?;\n", rune(clause[i])) {
			return i
		}
	}
	return -1
}

func quotedImplementationSymbol(span string) (string, bool) {
	value := strings.TrimSpace(strings.Trim(span, "`"))
	if proposalSourcePath(value) {
		return "", false
	}
	match := quotedCodePattern.FindStringSubmatch(value)
	if len(match) != 2 {
		return "", false
	}
	symbol := match[1]
	if dot := strings.LastIndexByte(symbol, '.'); dot >= 0 {
		symbol = symbol[dot+1:]
	}
	return symbol, identifierPattern.MatchString(symbol)
}

func codeLikeIdentifier(candidate string) bool {
	if !identifierPattern.MatchString(candidate) {
		return false
	}
	if strings.Contains(candidate, "_") {
		return true
	}
	hasLower := false
	hasInnerUpper := false
	for i := range len(candidate) {
		switch {
		case candidate[i] >= 'a' && candidate[i] <= 'z':
			hasLower = true
		case i > 0 && candidate[i] >= 'A' && candidate[i] <= 'Z':
			hasInnerUpper = true
		}
	}
	return hasLower && hasInnerUpper
}

func containsCandidateIdentifier(content string, symbols map[string]bool) bool {
	file := token.NewFileSet().AddFile("source.go", -1, len(content))
	var scanner goscanner.Scanner
	scanner.Init(file, []byte(content), nil, 0)
	for {
		_, tok, literal := scanner.Scan()
		if tok == token.EOF {
			return false
		}
		if tok == token.IDENT && symbols[literal] {
			return true
		}
	}
}

func proposalSourcePath(candidate string) bool {
	candidate = strings.TrimSpace(candidate)
	clean := pathpkg.Clean(candidate)
	if clean == "." || clean == ".." || clean != candidate || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") ||
		strings.Contains(clean, "\\") || strings.Contains(clean, "://") || strings.ContainsAny(clean, " \t\r\n?#") {
		return false
	}
	return sourceExtensions[strings.ToLower(pathpkg.Ext(clean))]
}

func readSourceFiles(ctx context.Context, reader Reader, paths []string) (map[string]string, error) {
	if bulk, ok := reader.(bulkReader); ok {
		contents, err := bulk.ReadFiles(ctx, paths)
		if err != nil {
			return nil, fmt.Errorf("read pinned source archive: %w", err)
		}
		return contents, nil
	}
	contents := make(map[string]string, len(paths))
	for _, path := range paths {
		content, found, err := reader.ReadFile(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("read pinned source %s: %w", path, err)
		}
		if found {
			contents[path] = content
		}
	}
	return contents, nil
}

func newPackageResolver(contents map[string]string, modulePaths, goPaths []string) packageResolver {
	modules := make([]moduleRoot, 0, len(modulePaths))
	for _, path := range modulePaths {
		modulePath := parseModulePath(contents[path])
		if modulePath == "" {
			continue
		}
		dir := pathpkg.Dir(path)
		if dir == "." {
			dir = ""
		}
		modules = append(modules, moduleRoot{dir: dir, path: modulePath})
	}
	sort.Slice(modules, func(i, j int) bool { return len(modules[i].dir) > len(modules[j].dir) })
	resolver := packageResolver{modules: modules, packageNames: map[string]string{}}
	conflicts := map[string]bool{}
	for _, path := range goPaths {
		file, err := parser.ParseFile(token.NewFileSet(), path, contents[path], parser.PackageClauseOnly)
		if err != nil || strings.HasSuffix(file.Name.Name, "_test") {
			continue
		}
		packagePath := resolver.packageID(path)
		if conflicts[packagePath] {
			continue
		}
		if previous := resolver.packageNames[packagePath]; previous != "" && previous != file.Name.Name {
			delete(resolver.packageNames, packagePath)
			conflicts[packagePath] = true
			continue
		}
		resolver.packageNames[packagePath] = file.Name.Name
	}
	return resolver
}

func parseModulePath(content string) string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || fields[0] != "module" {
			continue
		}
		value := fields[1]
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		return strings.TrimSpace(value)
	}
	return ""
}

func (r packageResolver) packageID(filePath string) string {
	dir := pathpkg.Dir(filePath)
	if dir == "." {
		dir = ""
	}
	for _, module := range r.modules {
		if module.dir != "" && dir != module.dir && !strings.HasPrefix(dir, module.dir+"/") {
			continue
		}
		relative := strings.TrimPrefix(strings.TrimPrefix(dir, module.dir), "/")
		if relative == "" {
			return "module:" + module.path
		}
		return "module:" + strings.TrimSuffix(module.path, "/") + "/" + relative
	}
	return "repo:" + dir
}

type sourceEvidenceVisitor struct {
	symbols         map[string]bool
	evidence        *sourceEvidence
	packageID       string
	imports         map[string]string
	hasDotImport    bool
	currentFunction string
}

func unwrapGenericCall(expr ast.Expr) ast.Expr {
	for {
		switch value := expr.(type) {
		case *ast.IndexExpr:
			expr = value.X
		case *ast.IndexListExpr:
			expr = value.X
		case *ast.ParenExpr:
			expr = value.X
		default:
			return expr
		}
	}
}

func (v *sourceEvidenceVisitor) Visit(node ast.Node) ast.Visitor {
	if node == nil {
		return nil
	}
	switch value := node.(type) {
	case *ast.FuncDecl:
		if v.symbols[value.Name.Name] {
			markPackage(v.evidence.definitions, value.Name.Name, v.packageID)
		}
		child := *v
		child.currentFunction = ""
		if value.Recv == nil {
			child.currentFunction = value.Name.Name
		}
		return &child
	case *ast.CallExpr:
		name, callPackage, ambiguous := "", "", false
		funExpr := unwrapGenericCall(value.Fun)
		switch fun := funExpr.(type) {
		case *ast.Ident:
			name = fun.Name
			switch {
			case fun.Obj != nil && fun.Obj.Kind != ast.Fun:
				ambiguous = true
			case fun.Obj == nil && v.hasDotImport:
				ambiguous = true
			default:
				callPackage = v.packageID
			}
		case *ast.SelectorExpr:
			name = fun.Sel.Name
			qualifier, ok := fun.X.(*ast.Ident)
			if ok && qualifier.Obj == nil {
				callPackage = v.imports[qualifier.Name]
			}
			ambiguous = callPackage == ""
		}
		if !v.symbols[name] || name == v.currentFunction && callPackage == v.packageID {
			return v
		}
		if ambiguous {
			v.evidence.ambiguousSelectors[name] = true
		} else if callPackage != "" {
			markPackage(v.evidence.calls, name, callPackage)
		}
	}
	return v
}

func inspectGoSource(path, content string, symbols map[string]bool, resolver packageResolver, evidence *sourceEvidence) (string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, content, 0)
	if err != nil {
		return "", err
	}
	packageID := resolver.packageID(path) + "#" + file.Name.Name
	imports := map[string]string{}
	hasDotImport := false
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		packagePath := "module:" + importPath
		packageName := resolver.packageNames[packagePath]
		if packageName == "" {
			packageName = pathpkg.Base(importPath)
		}
		alias := packageName
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		switch alias {
		case ".":
			hasDotImport = true
		case "", "_":
		default:
			imports[alias] = packagePath + "#" + packageName
		}
	}
	ast.Walk(&sourceEvidenceVisitor{
		symbols: symbols, evidence: evidence, packageID: packageID,
		imports: imports, hasDotImport: hasDotImport,
	}, file)
	return packageID, nil
}

func mergeAnchoredEvidence(target *sourceEvidence, source sourceEvidence, filePackage string, anchors map[string]map[string]bool, symbols map[string]bool) {
	for symbol := range symbols {
		for packageID := range source.definitions[symbol] {
			if anchors[symbol][packageID] {
				markPackage(target.definitions, symbol, packageID)
			}
		}
		for packageID := range source.calls[symbol] {
			if anchors[symbol][packageID] {
				markPackage(target.calls, symbol, packageID)
			}
		}
		if source.ambiguousSelectors[symbol] && anchors[symbol][filePackage] {
			target.ambiguousSelectors[symbol] = true
		}
	}
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
