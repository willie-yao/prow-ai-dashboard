package onboard

import (
	"context"
	"fmt"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
)

// completer is the subset of *ai.Client the generator needs.
type completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// promptSystemInstruction tells the model to write a project-specific prompt
// addendum grounded in the provided docs.
const promptSystemInstruction = `You write a project-specific knowledge addendum for an AI assistant that debugs CI test failures for a software project. The addendum is concatenated between a universal Prow base prompt and a JSON response schema, so write ONLY the project-specific middle: concise, factual markdown.

You are given the project's own documentation. Ground everything in it; do not invent components, file paths, or behaviors the docs don't support. Where the docs are silent (e.g. exact artifact paths), describe what to look for generically rather than guessing specifics.

Produce markdown with exactly these sections:

## Architecture
What the system under test is and how its main components relate. Keep it tight (a short list or a few sentences), focused on what helps explain a failure.

## Where the evidence lives
The artifacts a debugger should read first (build logs, per-component logs, resource dumps) and what each is good for. If the docs don't specify artifact layout, give the common Prow defaults (build-log.txt, the artifacts/ tree) and note they should be confirmed.

## Known transient / flake classes
Infrastructure flakes that should be classified transient (NOT turned into a "real bug" verdict): e.g. registry/image pull timeouts, quota or capacity errors, control-plane/API server still starting, DNS blips. Prefer classes the docs mention; add the common ones otherwise.

Rules: output only the markdown body starting at "## Architecture". No preamble, no code fences around the whole thing, no closing remarks.`

// generatePromptBody asks the model to draft the system.md body from the source
// docs. Returns the markdown body starting at "## Architecture".
func generatePromptBody(ctx context.Context, c completer, projectName string, docs []sourceDoc) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Project: %s\n\n", projectName)
	if len(docs) == 0 {
		b.WriteString("No documentation was found in the source repo. Draft a reasonable, clearly-generic addendum the maintainer can refine.\n")
	} else {
		b.WriteString("The project's documentation follows. Ground the addendum in it.\n\n")
		for _, d := range docs {
			fmt.Fprintf(&b, "===== FILE: %s =====\n%s\n\n", d.Path, d.Text)
		}
	}
	out, err := c.Complete(ctx, promptSystemInstruction, b.String())
	if err != nil {
		return "", err
	}
	body := sanitizePromptBody(out)
	if body == "" {
		return "", fmt.Errorf("model returned an empty prompt body")
	}
	// Require the three sections so malformed drafts fall back to the stub.
	for _, h := range []string{"## Architecture", "## Where the evidence lives", "## Known transient"} {
		if !strings.Contains(body, h) {
			return "", fmt.Errorf("generated prompt missing required section %q", h)
		}
	}
	return body, nil
}

// sanitizePromptBody trims stray wrapping code fences and leading prose the
// model sometimes adds before the first heading.
func sanitizePromptBody(s string) string {
	s = strings.TrimSpace(s)
	// Strip a single wrapping ```...``` fence if the whole body is fenced.
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimRight(s, "\n"), "```")
		s = strings.TrimSpace(s)
	}
	// Drop any preamble before the first markdown heading.
	if i := strings.Index(s, "## "); i > 0 {
		s = s[i:]
	}
	return strings.TrimSpace(s)
}

// composeGeneratedPrompt wraps a generated body with the same informational
// header the stub uses, so the file reads consistently.
func composeGeneratedPrompt(projectName, body string) string {
	return fmt.Sprintf(`# %s AI prompt addendum

This file is concatenated between the engine's universal Prow base prompt and
its JSON response schema. It was drafted automatically from the project's docs
by `+"`prow-ai-dashboard onboard`"+`; review and refine it, since prompt quality is
the biggest lever on analysis depth.

---

%s
`, projectName, body)
}

// Ensure *ai.Client satisfies completer at compile time.
var _ completer = (*ai.Client)(nil)
