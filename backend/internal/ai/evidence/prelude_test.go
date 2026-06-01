package evidence

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestBuildPrelude_OmitsEmptySections(t *testing.T) {
	// No build log URL, no failure body: just header + test + count + error.
	got := BuildPrelude(context.Background(), http.DefaultClient,
		&models.BuildResult{},
		&models.TestCase{Name: "TestFoo", FailureMessage: "boom"},
		3, Options{})

	for _, want := range []string{
		"Investigate this test failure",
		"Test: TestFoo",
		"Failed 3 consecutive times",
		"Error: boom",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prelude missing %q\n--- got ---\n%s", want, got)
		}
	}
	if strings.Contains(got, "Stack trace:") {
		t.Error("expected no Stack trace section when FailureBody is empty")
	}
	if strings.Contains(got, "Build Log") {
		t.Error("expected no Build Log section when URL is empty")
	}
}

func TestBuildPrelude_TruncatesStackToStackChars(t *testing.T) {
	body := strings.Repeat("x", 10_000)
	got := BuildPrelude(context.Background(), http.DefaultClient,
		&models.BuildResult{},
		&models.TestCase{Name: "T", FailureMessage: "m", FailureBody: body},
		1, Options{StackChars: 100})

	if !strings.Contains(got, "Stack trace:") {
		t.Fatal("missing Stack trace section")
	}
	if !strings.Contains(got, strings.Repeat("x", 100)+"...") {
		t.Error("expected stack to be truncated to 100 chars + ellipsis")
	}
	if strings.Contains(got, strings.Repeat("x", 101)) {
		t.Error("stack was not truncated")
	}
}

func TestBuildPrelude_RendersBuildLogTail(t *testing.T) {
	// 600 lines named L1..L600, BuildLogURL points at our test server.
	var lines []string
	for i := 1; i <= 600; i++ {
		lines = append(lines, "L"+itoa(i))
	}
	body := strings.Join(lines, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	got := BuildPrelude(context.Background(), http.DefaultClient,
		&models.BuildResult{BuildInfo: models.BuildInfo{BuildLogURL: srv.URL}},
		&models.TestCase{Name: "T", FailureMessage: "m"},
		1, Options{TailLines: 50, TailChars: 100_000})

	if !strings.Contains(got, "=== Build Log (last 50 lines) ===") {
		t.Error("missing Build Log header reflecting TailLines")
	}
	// Should include the last 50 lines (L551..L600) and NOT L1.
	if !strings.Contains(got, "L600") || !strings.Contains(got, "L551") {
		t.Error("expected tail to contain L551..L600")
	}
	if strings.Contains(got, "L1\n") {
		t.Error("expected leading lines to be dropped")
	}
}

func TestFetchLogTail_CapsToMaxChars(t *testing.T) {
	body := strings.Repeat("a", 5_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	got := FetchLogTail(context.Background(), http.DefaultClient, srv.URL, 1_000, 100)
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected ... suffix on overflow, got %q", got[max(0, len(got)-10):])
	}
	if len(got) != 103 {
		t.Errorf("expected 103-byte output (100 + ...), got %d", len(got))
	}
}

// helpers

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
