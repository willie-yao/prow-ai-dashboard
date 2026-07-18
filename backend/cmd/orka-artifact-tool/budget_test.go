package main

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
)

type budgetTestBrowser struct {
	artifacts.Browser
	content []byte
}

func (b *budgetTestBrowser) Read(_ context.Context, _ string, offset, length int) ([]byte, int64, error) {
	end := min(offset+length, len(b.content))
	return b.content[offset:end], int64(len(b.content)), nil
}

func TestScopeBudgetCapsArtifactReadsAndCountsResponses(t *testing.T) {
	budget := newScopeBudget(2, 10, 3)
	browser := &budgetBrowser{Browser: &budgetTestBrowser{content: []byte("hello")}, budget: budget}
	data, _, err := browser.Read(context.Background(), "file", 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hel" {
		t.Fatalf("data = %q, want clamped read", data)
	}
	if _, _, err := browser.Read(context.Background(), "file", 3, 2); err == nil {
		t.Fatal("read after budget exhaustion succeeded")
	}
	recorder := httptest.NewRecorder()
	writer := &budgetResponseWriter{ResponseWriter: recorder, budget: budget}
	if _, err := writer.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	callBudget := newScopeBudget(2, 10, 10)
	first := callBudget.reserveCall()
	second := callBudget.reserveCall()
	third := callBudget.reserveCall()
	if !first || !second || third {
		t.Fatal("call budget was not enforced")
	}
	model, gcs := budget.remaining()
	if model != 6 || gcs != 0 {
		t.Fatalf("remaining model=%d gcs=%d, want 6/0", model, gcs)
	}
}
