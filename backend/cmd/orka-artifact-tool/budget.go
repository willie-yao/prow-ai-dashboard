package main

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sync"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
)

const (
	defaultScopeCalls      = 256
	defaultScopeModelBytes = 8 << 20
	defaultScopeGCSBytes   = 64 << 20
)

type scopeBudget struct {
	mu         sync.Mutex
	callLimit  int
	modelLimit int
	gcsLimit   int
	calls      int
	modelUsed  int
	gcsUsed    int
}

func newScopeBudget(callLimit, modelLimit, gcsLimit int) *scopeBudget {
	return &scopeBudget{callLimit: callLimit, modelLimit: modelLimit, gcsLimit: gcsLimit}
}

func (b *scopeBudget) reserveCall() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.calls >= b.callLimit || b.modelUsed >= b.modelLimit || b.gcsUsed >= b.gcsLimit {
		return false
	}
	b.calls++
	return true
}

func (b *scopeBudget) remaining() (model, gcs int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(0, b.modelLimit-b.modelUsed), max(0, b.gcsLimit-b.gcsUsed)
}

func (b *scopeBudget) consumeModel(n int) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.modelUsed += n
	b.mu.Unlock()
}

func (b *scopeBudget) consumeGCS(n int) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.gcsUsed += n
	b.mu.Unlock()
}

type budgetResponseWriter struct {
	http.ResponseWriter
	budget *scopeBudget
}

func (w *budgetResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.budget.consumeModel(n)
	return n, err
}

type budgetBrowser struct {
	artifacts.Browser
	budget *scopeBudget
}

func (b *budgetBrowser) Read(ctx context.Context, file string, offset, length int) ([]byte, int64, error) {
	_, remaining := b.budget.remaining()
	if remaining == 0 {
		return nil, -1, fmt.Errorf("artifact byte budget exhausted")
	}
	length = min(length, remaining)
	data, size, err := b.Browser.Read(ctx, file, offset, length)
	b.budget.consumeGCS(len(data))
	return data, size, err
}

func (b *budgetBrowser) Tail(ctx context.Context, file string, lines, maxBytes int) (*artifacts.TailResult, error) {
	_, remaining := b.budget.remaining()
	if remaining == 0 {
		return nil, fmt.Errorf("artifact byte budget exhausted")
	}
	result, err := b.Browser.Tail(ctx, file, lines, min(maxBytes, remaining))
	if result != nil {
		b.budget.consumeGCS(len(result.Content))
	}
	return result, err
}

func (b *budgetBrowser) Grep(ctx context.Context, file string, re *regexp.Regexp, contextLines, maxMatches, maxLineLen int) (*artifacts.GrepResult, error) {
	_, remaining := b.budget.remaining()
	if remaining == 0 {
		return nil, fmt.Errorf("artifact byte budget exhausted")
	}
	result, err := b.Browser.Grep(ctx, file, re, contextLines, maxMatches, maxLineLen)
	if result != nil {
		scanned := int(result.BytesScanned)
		b.budget.consumeGCS(min(scanned, remaining))
		if scanned > remaining {
			return nil, fmt.Errorf("artifact byte budget exhausted")
		}
	}
	return result, err
}
