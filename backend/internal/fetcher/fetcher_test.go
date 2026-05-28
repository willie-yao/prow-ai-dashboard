package fetcher

import (
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

func TestAIEndpoint_PrefersYAMLOverEnv(t *testing.T) {
	t.Setenv("AI_ENDPOINT", "https://env.example/v1/chat")
	cfg := &project.Config{AI: &project.AI{Endpoint: "https://yaml.example/v1/chat"}}
	if got := aiEndpoint(cfg); got != "https://yaml.example/v1/chat" {
		t.Errorf("aiEndpoint: got %q, want yaml value", got)
	}
}

func TestAIEndpoint_FallsBackToEnvWhenYAMLBlank(t *testing.T) {
	t.Setenv("AI_ENDPOINT", "https://env.example/v1/chat")
	cfg := &project.Config{AI: &project.AI{}}
	if got := aiEndpoint(cfg); got != "https://env.example/v1/chat" {
		t.Errorf("aiEndpoint: got %q, want env value", got)
	}
}

func TestAIEndpoint_EmptyWhenNothingSet(t *testing.T) {
	t.Setenv("AI_ENDPOINT", "")
	cfg := &project.Config{AI: &project.AI{}}
	if got := aiEndpoint(cfg); got != "" {
		t.Errorf("aiEndpoint: got %q, want empty", got)
	}
}

func TestAIEndpoint_NilAIBlock(t *testing.T) {
	t.Setenv("AI_ENDPOINT", "https://env.example/v1/chat")
	cfg := &project.Config{}
	if got := aiEndpoint(cfg); got != "https://env.example/v1/chat" {
		t.Errorf("aiEndpoint: got %q, want env value when AI block is nil", got)
	}
}

func TestAIModel_PrefersYAMLOverEnv(t *testing.T) {
	t.Setenv("AI_MODEL", "env-model")
	cfg := &project.Config{AI: &project.AI{Model: "yaml-model"}}
	if got := aiModel(cfg); got != "yaml-model" {
		t.Errorf("aiModel: got %q, want yaml value", got)
	}
}

func TestAIModel_FallsBackToEnvWhenYAMLBlank(t *testing.T) {
	t.Setenv("AI_MODEL", "env-model")
	cfg := &project.Config{AI: &project.AI{}}
	if got := aiModel(cfg); got != "env-model" {
		t.Errorf("aiModel: got %q, want env value", got)
	}
}

func TestAIModel_EmptyWhenNothingSet(t *testing.T) {
	t.Setenv("AI_MODEL", "")
	cfg := &project.Config{AI: &project.AI{}}
	if got := aiModel(cfg); got != "" {
		t.Errorf("aiModel: got %q, want empty", got)
	}
}

func TestAIModel_NilAIBlock(t *testing.T) {
	t.Setenv("AI_MODEL", "env-model")
	cfg := &project.Config{}
	if got := aiModel(cfg); got != "env-model" {
		t.Errorf("aiModel: got %q, want env value when AI block is nil", got)
	}
}
