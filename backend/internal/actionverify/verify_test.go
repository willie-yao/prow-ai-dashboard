package actionverify

import (
	"context"
	"fmt"
	"sort"
	"testing"
)

type fakeReader map[string]string

func (f fakeReader) ListTree(context.Context) ([]string, error) {
	paths := make([]string, 0, len(f))
	for path := range f {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}
func (f fakeReader) ReadFile(_ context.Context, path string) (string, bool, error) {
	value, ok := f[path]
	return value, ok, nil
}

func TestVerifyDetectsExistingImplementationAndCall(t *testing.T) {
	reader := fakeReader{
		"internal/asomigration/labels.go": "package asomigration\nfunc LabelCRDsForClusterctlUpgrade() error { return nil }\n",
		"test/e2e/capi_test.go":           "package e2e\nimport \"example/asomigration\"\nfunc test() { _ = asomigration.LabelCRDsForClusterctlUpgrade() }\n",
	}
	verifyState(t, reader, Input{
		Proposal:      "Implement `LabelCRDsForClusterctlUpgrade`.",
		RelevantFiles: []string{"internal/asomigration/labels.go", "test/e2e/capi_test.go"},
	}, StateAlreadyPresent)
}

func TestVerifyFindsInvocationOutsideGroundedPaths(t *testing.T) {
	reader := fakeReader{
		"internal/asomigration/labels.go": "package asomigration\nfunc LabelCRDsForClusterctlUpgrade() error { return nil }\n",
		"test/e2e/capi_test.go":           "package e2e\nimport \"example/asomigration\"\nfunc test() { _ = asomigration.LabelCRDsForClusterctlUpgrade() }\n",
	}
	verifyState(t, reader, Input{
		Proposal:      "Implement `LabelCRDsForClusterctlUpgrade`.",
		RelevantFiles: []string{"internal/asomigration/labels.go"},
	}, StateAlreadyPresent)
}

func TestVerifyAllowsMissingImplementationAfterExhaustiveRead(t *testing.T) {
	verifyState(t, fakeReader{"main.go": "package main\n"}, Input{
		Proposal: "Implement MissingHelper.", RelevantFiles: []string{"main.go"},
	}, StateUnresolved)
}

func TestVerifyIgnoresCommentedAndStringSymbols(t *testing.T) {
	reader := fakeReader{"main.go": "package p\n// func ExistingFix() {}\nvar x = \"ExistingFix()\"\n"}
	verifyState(t, reader, Input{Proposal: "Implement ExistingFix.", RelevantFiles: []string{"main.go"}}, StateUnresolved)
}

func TestVerifyRequiresEveryProposedSymbol(t *testing.T) {
	reader := fakeReader{"main.go": "package p\nfunc FooHelper() {}\nfunc x(){ FooHelper() }\n"}
	verifyState(t, reader, Input{
		Proposal: "Implement FooHelper and add BarHelper.", RelevantFiles: []string{"main.go"},
	}, StateUnresolved)
}

func TestVerifyDoesNotMixUnrelatedSelectorPackage(t *testing.T) {
	reader := fakeReader{
		"a/helper.go": "package a\nfunc ReconcileThing() {}\n",
		"b/use.go":    "package b\nimport \"example/other\"\nfunc use(){ other.ReconcileThing() }\n",
	}
	verifyState(t, reader, Input{
		Proposal: "Implement ReconcileThing.", RelevantFiles: []string{"a/helper.go", "b/use.go"},
	}, StateInconclusive)
}

func TestVerifyResolvesImportAliasToPackageBasename(t *testing.T) {
	reader := fakeReader{
		"asomigration/fix.go": "package asomigration\nfunc ExistingFix(){}\n",
		"e2e/use.go":          "package e2e\nimport migration \"example/asomigration\"\nfunc x(){ migration.ExistingFix() }\n",
	}
	verifyState(t, reader, Input{
		Proposal: "Implement ExistingFix.", RelevantFiles: []string{"asomigration/fix.go", "e2e/use.go"},
	}, StateAlreadyPresent)
}

func TestVerifyShadowedImportDoesNotCountAsPackageInvocation(t *testing.T) {
	reader := fakeReader{
		"a/fix.go": "package a\nfunc ExistingFix(){}\n",
		"b/use.go": "package b\nimport \"example/a\"\nfunc x(){ a := runner{}; a.ExistingFix() }\ntype runner struct{}\nfunc (runner) ExistingFix(){}\n",
	}
	verifyState(t, reader, Input{
		Proposal: "Implement ExistingFix.", RelevantFiles: []string{"a/fix.go", "b/use.go"},
	}, StateInconclusive)
}

func TestVerifyLocalReceiverCallIsInconclusive(t *testing.T) {
	reader := fakeReader{
		"runner.go": "package p\ntype runner struct{}\nfunc (runner) ExistingFix(){}\nfunc x(){ r := runner{}; r.ExistingFix() }\n",
	}
	verifyState(t, reader, Input{
		Proposal: "Implement ExistingFix.", RelevantFiles: []string{"runner.go"},
	}, StateInconclusive)
}

func TestVerifyMissingGroundedPathIsInconclusive(t *testing.T) {
	verifyState(t, fakeReader{"main.go": "package main\n"}, Input{
		Proposal: "Implement MissingHelper.", RelevantFiles: []string{"missing.go"},
	}, StateInconclusive)
}

func TestVerifyAddCallForm(t *testing.T) {
	verifyState(t, fakeReader{"p.go": "package p\nfunc ExistingFix(){}\nfunc x(){ExistingFix()}\n"}, Input{
		Proposal: "Add a call to ExistingFix.", RelevantFiles: []string{"p.go"},
	}, StateAlreadyPresent)
}

func TestVerifyNonGoOnlyIsInconclusive(t *testing.T) {
	verifyState(t, fakeReader{"config.yaml": "ExistingFix: true", "main.go": "package main\n"}, Input{
		Proposal: "Implement ExistingFix.", RelevantFiles: []string{"config.yaml"},
	}, StateInconclusive)
}

type oversizedReader struct {
	fakeReader
}

func (r oversizedReader) ListTree(context.Context) ([]string, error) {
	paths := make([]string, maxExhaustiveGoFiles+1)
	for i := range paths {
		paths[i] = fmt.Sprintf("pkg/file-%04d.go", i)
	}
	return paths, nil
}

func TestVerifyOversizedTreeIsInconclusive(t *testing.T) {
	verifyState(t, oversizedReader{fakeReader{"main.go": "package main\n"}}, Input{
		Proposal: "Implement MissingHelper.", RelevantFiles: []string{"main.go"},
	}, StateInconclusive)
}

func verifyState(t *testing.T, reader Reader, input Input, want string) {
	t.Helper()
	result, err := Verify(context.Background(), reader, input)
	if err != nil || result.State != want {
		t.Fatalf("result=%+v err=%v, want %s", result, err, want)
	}
}
