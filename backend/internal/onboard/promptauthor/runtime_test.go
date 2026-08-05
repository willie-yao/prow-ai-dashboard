package promptauthor

import (
	"context"
	"strings"
	"testing"

	agentruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

type fakeAgent struct {
	got agentruntime.GenerateSpec
	res agentruntime.GenerateResult
	err error
}

func (f *fakeAgent) Generate(_ context.Context, spec agentruntime.GenerateSpec) (agentruntime.GenerateResult, error) {
	f.got = spec
	return f.res, f.err
}

func validPrompt() string {
	var b strings.Builder
	b.WriteString("# Project prompt\n\n---\n\n")
	for _, heading := range requiredHeadings {
		b.WriteString(heading + "\n\n- Grounded project-specific guidance.\n\n")
	}
	return b.String()
}

func TestOpenCodeRuntimeGenerate(t *testing.T) {
	agent := &fakeAgent{res: agentruntime.GenerateResult{Files: map[string]string{OutputPath: validPrompt()}, Diff: "diff", Output: "safe"}}
	r := &OpenCodeRuntime{Agent: agent}
	got, err := r.Generate(context.Background(), Spec{
		Repo:        agentruntime.RepoRef{Owner: "o", Name: "n", Ref: "sha"},
		Instruction: "Generate the prompt.", Model: "model", Endpoint: "https://example.test/v1", Token: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Body == "" || got.Runtime != "opencode" || got.Output != "safe" {
		t.Fatalf("result = %+v", got)
	}
	if agent.got.AllowBash || !strings.Contains(agent.got.Instruction, SkillName) || agent.got.Skills[SkillName] == "" {
		t.Fatalf("agent spec = %+v", agent.got)
	}
}

func TestOpenCodeRuntimeRejectsUnsafeChanges(t *testing.T) {
	tests := map[string]agentruntime.GenerateResult{
		"missing prompt": {Files: map[string]string{"other.txt": "x"}},
		"extra file":     {Files: map[string]string{OutputPath: validPrompt(), "other.txt": "x"}},
		"deletion":       {Files: map[string]string{OutputPath: validPrompt()}, Diff: "deleted file mode 100644"},
		"invalid prompt": {Files: map[string]string{OutputPath: "## Architecture\n- only one section"}},
	}
	for name, result := range tests {
		t.Run(name, func(t *testing.T) {
			r := &OpenCodeRuntime{Agent: &fakeAgent{res: result}}
			_, err := r.Generate(context.Background(), Spec{Repo: agentruntime.RepoRef{Owner: "o", Name: "n", Ref: "sha"}, Instruction: "x"})
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestDiffDestructiveMetadataUsesExactLines(t *testing.T) {
	if diffHasDestructiveChange("+deleted file mode is documentation") {
		t.Fatal("added content was treated as Git deletion metadata")
	}
	if !diffHasDestructiveChange("rename from old.md\nrename to new.md") {
		t.Fatal("rename metadata was not rejected")
	}
}

func TestValidatePromptQuality(t *testing.T) {
	if err := Validate(validPrompt()); err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(validPrompt(), "## Architecture\n\n- Grounded project-specific guidance.", "## Architecture\n\n- TODO: fill this.", 1)
	if err := Validate(bad); err == nil {
		t.Fatal("expected TODO-only architecture to fail")
	}
	oversized := strings.Repeat(" ", maxBytes) + validPrompt()
	if err := Validate(oversized); err == nil {
		t.Fatal("leading whitespace bypassed the byte limit")
	}
	inline := strings.Replace(validPrompt(), "# Project prompt", "# Project prompt\nSee ## Architecture below.", 1)
	inline = strings.Replace(inline, "## Architecture\n\n- Grounded project-specific guidance.", "## Architecture\n", 1)
	if err := Validate(inline); err == nil {
		t.Fatal("inline heading mention bypassed empty section validation")
	}
}
