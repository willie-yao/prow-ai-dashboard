package actionverify

import (
	"context"
	"testing"
)

type fakeReader map[string]string

func (f fakeReader) ListTree(context.Context) ([]string, error) { return nil, nil }
func (f fakeReader) ReadFile(_ context.Context, path string) (string, bool, error) {
	value, ok := f[path]
	return value, ok, nil
}

func TestVerifyDetectsExistingImplementationAndCall(t *testing.T) {
	reader := fakeReader{
		"internal/asomigration/labels.go": "package asomigration\nfunc LabelCRDsForClusterctlUpgrade() error { return nil }\n",
		"test/e2e/capi_test.go":           "package e2e\nfunc test() { _ = asomigration.LabelCRDsForClusterctlUpgrade() }\n",
	}
	result, err := Verify(context.Background(), reader, Input{
		Proposal:      "Implement `LabelCRDsForClusterctlUpgrade`.",
		RelevantFiles: []string{"internal/asomigration/labels.go", "test/e2e/capi_test.go"},
	})
	if err != nil || result.State != StateAlreadyPresent {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestVerifyAllowsMissingImplementation(t *testing.T) {
	result, err := Verify(context.Background(), fakeReader{"main.go": "package main\n"}, Input{Proposal: "Implement MissingHelper.", RelevantFiles: []string{"main.go"}})
	if err != nil || result.State != StateUnresolved {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestVerifyIgnoresCommentedAndStringSymbols(t *testing.T) {
	reader := fakeReader{"main.go": "package p\n// func ExistingFix() {}\nvar x = \"ExistingFix()\"\n"}
	result, err := Verify(context.Background(), reader, Input{Proposal: "Implement ExistingFix.", RelevantFiles: []string{"main.go"}})
	if err != nil || result.State != StateUnresolved {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestVerifyRequiresEveryProposedSymbol(t *testing.T) {
	reader := fakeReader{"main.go": "package p\nfunc Foo() {}\nfunc x(){ Foo() }\n"}
	result, err := Verify(context.Background(), reader, Input{Proposal: "Implement FooHelper and add BarHelper.", RelevantFiles: []string{"main.go"}})
	if err != nil || result.State != StateUnresolved {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
