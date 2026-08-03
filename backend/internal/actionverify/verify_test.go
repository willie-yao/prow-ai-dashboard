package actionverify

import (
	"context"
	"fmt"
	"sort"
	"strings"
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
		"go.mod":                          "module example\n",
		"internal/asomigration/labels.go": "package asomigration\nfunc LabelCRDsForClusterctlUpgrade() error { return nil }\n",
		"test/e2e/capi_test.go":           "package e2e\nimport \"example/internal/asomigration\"\nfunc test() { _ = asomigration.LabelCRDsForClusterctlUpgrade() }\n",
	}
	verifyState(t, reader, Input{
		Proposal:      "Implement `LabelCRDsForClusterctlUpgrade`.",
		RelevantFiles: []string{"internal/asomigration/labels.go", "test/e2e/capi_test.go"},
	}, StateAlreadyPresent)
}

func TestVerifyFindsInvocationOutsideGroundedPaths(t *testing.T) {
	reader := fakeReader{
		"go.mod":                          "module example\n",
		"internal/asomigration/labels.go": "package asomigration\nfunc LabelCRDsForClusterctlUpgrade() error { return nil }\n",
		"test/e2e/capi_test.go":           "package e2e\nimport \"example/internal/asomigration\"\nfunc test() { _ = asomigration.LabelCRDsForClusterctlUpgrade() }\n",
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

func TestVerifyDoesNotMixPackagesWithSameDeclaredName(t *testing.T) {
	reader := fakeReader{
		"go.mod":         "module example\n",
		"a/helper.go":    "package util\nfunc ReconcileThing() {}\n",
		"b/use.go":       "package b\nimport \"example/c/util\"\nfunc use(){ util.ReconcileThing() }\n",
		"c/util/util.go": "package util\n",
	}
	verifyState(t, reader, Input{
		Proposal: "Implement ReconcileThing.", RelevantFiles: []string{"a/helper.go", "b/use.go"},
	}, StateInconclusive)
}

func TestVerifyResolvesImportAliasToPackageBasename(t *testing.T) {
	reader := fakeReader{
		"go.mod":              "module example\n",
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

func TestVerifyIgnoresBacktickedSemanticVersion(t *testing.T) {
	verifyState(t, fakeReader{"main.go": "package main\n"}, Input{
		Proposal: "Implement MissingHelper for `v1.13.3`.", RelevantFiles: []string{"main.go"},
	}, StateUnresolved)
}

func TestVerifyExhaustivePassDoesNotUseUnrelatedPackage(t *testing.T) {
	reader := fakeReader{
		"go.mod":       "module example\n",
		"target/a.go":  "package target\n",
		"other/fix.go": "package other\nfunc ValidateConfig(){}\nfunc use(){ ValidateConfig() }\n",
	}
	verifyState(t, reader, Input{
		Proposal: "Implement ValidateConfig.", RelevantFiles: []string{"target/a.go"},
	}, StateUnresolved)
}

func TestVerifyExhaustivePassUsesGroundedPackage(t *testing.T) {
	reader := fakeReader{
		"go.mod":           "module example\n",
		"target/a.go":      "package target\n",
		"target/helper.go": "package target\nfunc ValidateConfig(){}\nfunc use(){ ValidateConfig() }\n",
	}
	verifyState(t, reader, Input{
		Proposal: "Implement ValidateConfig.", RelevantFiles: []string{"target/a.go"},
	}, StateAlreadyPresent)
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
	paths[0] = "main.go"
	for i := 1; i < len(paths); i++ {
		paths[i] = fmt.Sprintf("pkg/file-%04d.go", i)
	}
	return paths, nil
}

func TestVerifyOversizedTreeIsInconclusive(t *testing.T) {
	result, err := Verify(context.Background(), oversizedReader{fakeReader{"main.go": "package main\n"}}, Input{
		Proposal: "Implement MissingHelper.", RelevantFiles: []string{"main.go"},
	})
	if err != nil || result.State != StateInconclusive || !strings.Contains(result.Reason, "too large") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func verifyState(t *testing.T, reader Reader, input Input, want string) {
	t.Helper()
	result, err := Verify(context.Background(), reader, input)
	if err != nil || result.State != want {
		t.Fatalf("result=%+v err=%v, want %s", result, err, want)
	}
}

type bulkFakeReader struct {
	fakeReader
	bulkCalls int
	readCalls int
}

func (r *bulkFakeReader) ReadFile(ctx context.Context, path string) (string, bool, error) {
	r.readCalls++
	return r.fakeReader.ReadFile(ctx, path)
}

func (r *bulkFakeReader) ReadFiles(_ context.Context, paths []string) (map[string]string, error) {
	r.bulkCalls++
	files := make(map[string]string, len(paths))
	for _, path := range paths {
		if content, ok := r.fakeReader[path]; ok {
			files[path] = content
		}
	}
	return files, nil
}

func TestVerifyUsesBulkPinnedSourceRead(t *testing.T) {
	reader := &bulkFakeReader{fakeReader: fakeReader{
		"go.mod":  "module example\n",
		"main.go": "package main\n",
	}}
	verifyState(t, reader, Input{
		Proposal: "Implement MissingHelper.", RelevantFiles: []string{"main.go"},
	}, StateUnresolved)
	if reader.bulkCalls != 1 || reader.readCalls != 0 {
		t.Fatalf("bulk calls = %d, file calls = %d", reader.bulkCalls, reader.readCalls)
	}
}

func TestVerifyIgnoresBacktickedSourceURL(t *testing.T) {
	verifyState(t, fakeReader{"main.go": "package main\n"}, Input{
		Proposal: "Implement MissingHelper using `https://example.test/main.go`.", RelevantFiles: []string{"main.go"},
	}, StateUnresolved)
}

func TestVerifyFindsCodeLikeSymbolAfterProse(t *testing.T) {
	reader := fakeReader{
		"main.go": "package main\nfunc ExistingFix(){}\nfunc use(){ ExistingFix() }\n",
	}
	for _, proposal := range []string{
		"Implement the missing ExistingFix helper.",
		"Implement validation by calling ExistingFix.",
	} {
		verifyState(t, reader, Input{Proposal: proposal, RelevantFiles: []string{"main.go"}}, StateAlreadyPresent)
	}
}

func TestVerifyRejectsAmbiguousProseSymbol(t *testing.T) {
	verifyState(t, fakeReader{"main.go": "package main\n"}, Input{
		Proposal: "Implement validation for this failure.", RelevantFiles: []string{"main.go"},
	}, StateInconclusive)
}

func TestVerifySkipsMalformedUnrelatedGoFixture(t *testing.T) {
	reader := fakeReader{
		"main.go":         "package main\n",
		"testdata/bad.go": "package broken\n// MissingHelper is only mentioned in a comment.\nfunc {\n",
	}
	verifyState(t, reader, Input{
		Proposal: "Implement MissingHelper.", RelevantFiles: []string{"main.go"},
	}, StateUnresolved)
}

func TestVerifyDoesNotTreatSourcePathAsSymbol(t *testing.T) {
	reader := fakeReader{
		"go.mod":                    "module example\n",
		"pkg/fix.go":                "package pkg\nfunc ExistingFix(){}\nfunc use(){ ExistingFix() }\n",
		"pkg/machine_controller.go": "package pkg\n",
	}
	verifyState(t, reader, Input{
		Proposal: "Implement ExistingFix in `pkg/machine_controller.go`.", RelevantFiles: []string{"pkg/fix.go"},
	}, StateAlreadyPresent)
}

func TestVerifyDoesNotCountDirectRecursionAsInvocation(t *testing.T) {
	reader := fakeReader{
		"main.go": "package main\nfunc ExistingFix(){ ExistingFix() }\n",
	}
	verifyState(t, reader, Input{
		Proposal: "Add a call to ExistingFix.", RelevantFiles: []string{"main.go"},
	}, StateUnresolved)
}

func TestVerifyIgnoresCamelCaseProseAfterSymbol(t *testing.T) {
	reader := fakeReader{
		"main.go": "package main\nfunc ExistingFix(){}\nfunc use(){ ExistingFix() }\n",
	}
	verifyState(t, reader, Input{
		Proposal: "Implement ExistingFix using GitHub APIs.", RelevantFiles: []string{"main.go"},
	}, StateAlreadyPresent)
}

func TestVerifyAcceptsShortBacktickedSymbol(t *testing.T) {
	reader := fakeReader{
		"main.go": "package main\nfunc Do(){}\nfunc use(){ Do() }\n",
	}
	verifyState(t, reader, Input{
		Proposal: "Implement `Do`.", RelevantFiles: []string{"main.go"},
	}, StateAlreadyPresent)
}

func TestVerifyAcceptsBacktickedCallNotation(t *testing.T) {
	reader := fakeReader{
		"main.go": "package main\nfunc ExistingFix(){}\nfunc use(){ ExistingFix() }\n",
	}
	for _, proposal := range []string{
		"Implement `ExistingFix()`.",
		"Implement `pkg.ExistingFix()`.",
	} {
		verifyState(t, reader, Input{Proposal: proposal, RelevantFiles: []string{"main.go"}}, StateAlreadyPresent)
	}
}

func TestVerifyGenericFunctionInvocation(t *testing.T) {
	for _, source := range []string{
		"package main\nfunc ExistingFix[T any](){}\nfunc use(){ ExistingFix[int]() }\n",
		"package main\nfunc ExistingFix[A, B any](){}\nfunc use(){ ExistingFix[int, string]() }\n",
	} {
		verifyState(t, fakeReader{"main.go": source}, Input{
			Proposal: "Implement ExistingFix.", RelevantFiles: []string{"main.go"},
		}, StateAlreadyPresent)
	}
}

func TestVerifyRequiresEverySymbolInSingleClause(t *testing.T) {
	reader := fakeReader{"main.go": "package main\nfunc ExistingFix(){}\nfunc use(){ ExistingFix() }\n"}
	verifyState(t, reader, Input{
		Proposal: "Implement ExistingFix and MissingHelper.", RelevantFiles: []string{"main.go"},
	}, StateUnresolved)
}

func TestVerifyBuildConstrainedEvidenceIsInconclusive(t *testing.T) {
	for name, constrainedFile := range map[string]string{
		"filename":  "target/fix_windows.go",
		"directive": "target/fix_tagged.go",
	} {
		t.Run(name, func(t *testing.T) {
			content := "package target\nfunc ExistingFix(){}\nfunc use(){ ExistingFix() }\n"
			if name == "directive" {
				content = "//go:build custom\n\n" + content
			}
			reader := fakeReader{
				"go.mod":         "module example\n",
				"target/main.go": "package target\n",
				constrainedFile:  content,
			}
			verifyState(t, reader, Input{
				Proposal: "Implement ExistingFix.", RelevantFiles: []string{"target/main.go"},
			}, StateInconclusive)
		})
	}
}

func TestVerifyDoesNotCombineMutuallyExclusiveFiles(t *testing.T) {
	reader := fakeReader{
		"go.mod":                "module example\n",
		"target/main.go":        "package target\n",
		"target/fix_windows.go": "package target\nfunc ExistingFix(){}\n",
		"target/use_linux.go":   "package target\nfunc use(){ ExistingFix() }\n",
	}
	verifyState(t, reader, Input{
		Proposal: "Implement ExistingFix.", RelevantFiles: []string{"target/main.go"},
	}, StateInconclusive)
}

func TestVerifyRequiresMixedQuotedAndUnquotedSymbols(t *testing.T) {
	reader := fakeReader{"main.go": "package main\nfunc ExistingFix(){}\nfunc use(){ ExistingFix() }\n"}
	verifyState(t, reader, Input{
		Proposal: "Implement MissingHelper and call `ExistingFix`.", RelevantFiles: []string{"main.go"},
	}, StateUnresolved)
}

func TestVerifyUsesDeclaredPackageNameForVersionedImport(t *testing.T) {
	reader := fakeReader{
		"go.mod":     "module example.com/lib/v2\n",
		"fix.go":     "package lib\nfunc ExistingFix(){}\n",
		"sub/use.go": "package sub\nimport \"example.com/lib/v2\"\nfunc use(){ lib.ExistingFix() }\n",
	}
	verifyState(t, reader, Input{
		Proposal: "Implement ExistingFix.", RelevantFiles: []string{"fix.go", "sub/use.go"},
	}, StateAlreadyPresent)
}

func TestVerifyAllowsMissingSymbolInConstrainedGroundedFile(t *testing.T) {
	reader := fakeReader{
		"go.mod":                 "module example\n",
		"target/main_windows.go": "package target\n",
	}
	verifyState(t, reader, Input{
		Proposal: "Implement MissingHelper.", RelevantFiles: []string{"target/main_windows.go"},
	}, StateUnresolved)
}
