package runtime

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestApplyDiffReconstructsChangedFiles(t *testing.T) {
	repo := initRepo(t)
	diff := "diff --git a/orig.txt b/orig.txt\n" +
		"--- a/orig.txt\n" +
		"+++ b/orig.txt\n" +
		"@@ -1 +1 @@\n" +
		"-base\n" +
		"+patched\n" +
		"diff --git a/new.txt b/new.txt\n" +
		"new file mode 100644\n" +
		"--- /dev/null\n" +
		"+++ b/new.txt\n" +
		"@@ -0,0 +1 @@\n" +
		"+added\n"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	files, out, err := ApplyDiff(ctx, RepoRef{Owner: "o", Name: "n", Ref: "main", CloneURL: repo}, diff)
	if err != nil {
		t.Fatal(err)
	}
	if files["orig.txt"] != "patched\n" || files["new.txt"] != "added\n" {
		t.Fatalf("files = %+v", files)
	}
	if !strings.Contains(out, "patched") || !strings.Contains(out, "new.txt") {
		t.Fatalf("diff = %q", out)
	}
}

func TestApplyDiffRejectsNonMatchingAndDeletion(t *testing.T) {
	repo := initRepo(t)
	bad := "diff --git a/orig.txt b/orig.txt\n--- a/orig.txt\n+++ b/orig.txt\n@@ -1 +1 @@\n-not present\n+whatever\n"
	if _, _, err := ApplyDiff(context.Background(), RepoRef{Ref: "main", CloneURL: repo}, bad); err == nil {
		t.Fatal("non-matching patch was accepted")
	}
	deletion := "diff --git a/orig.txt b/orig.txt\ndeleted file mode 100644\n--- a/orig.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-base\n"
	if _, _, err := ApplyDiff(context.Background(), RepoRef{Ref: "main", CloneURL: repo}, deletion); err == nil || !strings.Contains(err.Error(), "unsupported D") {
		t.Fatalf("deletion error = %v", err)
	}
}

func TestApplyDiffEmpty(t *testing.T) {
	files, diff, err := ApplyDiff(context.Background(), RepoRef{}, "")
	if err != nil || len(files) != 0 || diff != "" {
		t.Fatalf("files=%v diff=%q err=%v", files, diff, err)
	}
}
