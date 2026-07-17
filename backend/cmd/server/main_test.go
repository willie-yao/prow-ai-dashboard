package main

import (
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

func TestTrustedOrigins_DerivesRedirectHost(t *testing.T) {
	got := trustedOrigins("https://dash.example.net/api/auth/callback", "https://alt.example, other.example")
	want := map[string]bool{"dash.example.net": true, "https://alt.example": true, "other.example": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want keys %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected origin %q in %v", g, got)
		}
	}
}

func TestTrustedOrigins_EmptyRedirect(t *testing.T) {
	if got := trustedOrigins("", ""); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
	got := trustedOrigins("", "only.example")
	if len(got) != 1 || got[0] != "only.example" {
		t.Errorf("got %v, want [only.example]", got)
	}
}

func TestEmailReplyConfiguration(t *testing.T) {
	cfg := &project.Config{Notifications: &project.Notifications{Email: &project.EmailNotifications{
		Enabled: true, ActionLinks: true,
		Inbound: &project.EmailInbound{
			Enabled: true, ReplyTo: "{token}@replies.example.com",
			Maintainers: map[string]string{"alice": "Alice <alice@example.com>"},
		},
	}}}
	t.Setenv("EMAIL_REPLY_TOKEN_SECRET", strings.Repeat("r", 32))
	t.Setenv("EMAIL_INBOUND_WEBHOOK_SECRET", strings.Repeat("w", 32))
	t.Setenv("GITHUB_READ_TOKEN", "read-token")
	signer, err := emailReplySigner(cfg)
	if err != nil || signer == nil {
		t.Fatalf("signer=%v err=%v", signer, err)
	}
	options, err := emailReplyOptions(cfg, signer, []string{"alice"}, "oauth")
	if err != nil {
		t.Fatal(err)
	}
	if options.Maintainers["alice@example.com"] != "alice" || options.GenerationToken != "read-token" {
		t.Fatalf("options = %+v", options)
	}
}

func TestEmailReplyConfigurationRequiresSecrets(t *testing.T) {
	cfg := &project.Config{Notifications: &project.Notifications{Email: &project.EmailNotifications{
		Enabled: true, ActionLinks: true,
		Inbound: &project.EmailInbound{Enabled: true, ReplyTo: "{token}@replies.example.com", Maintainers: map[string]string{"alice": "alice@example.com"}},
	}}}
	if _, err := emailReplySigner(cfg); err == nil || !strings.Contains(err.Error(), "at least 32") {
		t.Fatalf("signer err=%v", err)
	}
	t.Setenv("EMAIL_REPLY_TOKEN_SECRET", strings.Repeat("r", 32))
	signer, err := emailReplySigner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := emailReplyOptions(cfg, signer, []string{"alice"}, "oauth"); err == nil || !strings.Contains(err.Error(), "EMAIL_INBOUND_WEBHOOK_SECRET") {
		t.Fatalf("options err=%v", err)
	}
}

func TestEmailReplyConfigurationRequiresAdminMapping(t *testing.T) {
	cfg := &project.Config{Notifications: &project.Notifications{Email: &project.EmailNotifications{
		Enabled: true, ActionLinks: true,
		Inbound: &project.EmailInbound{Enabled: true, ReplyTo: "{token}@replies.example.com", Maintainers: map[string]string{"alice": "alice@example.com"}},
	}}}
	t.Setenv("EMAIL_REPLY_TOKEN_SECRET", strings.Repeat("r", 32))
	t.Setenv("EMAIL_INBOUND_WEBHOOK_SECRET", strings.Repeat("w", 32))
	signer, err := emailReplySigner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := emailReplyOptions(cfg, signer, []string{"bob"}, "oauth"); err == nil || !strings.Contains(err.Error(), "ADMIN_LOGINS") {
		t.Fatalf("options err=%v", err)
	}
}

func TestEmailReplyConfigurationDoesNotUseBotTokenForGeneration(t *testing.T) {
	cfg := &project.Config{Notifications: &project.Notifications{Email: &project.EmailNotifications{
		Enabled: true, ActionLinks: true,
		Inbound: &project.EmailInbound{Enabled: true, ReplyTo: "{token}@replies.example.com", Maintainers: map[string]string{"alice": "alice@example.com"}},
	}}}
	t.Setenv("EMAIL_REPLY_TOKEN_SECRET", strings.Repeat("r", 32))
	t.Setenv("EMAIL_INBOUND_WEBHOOK_SECRET", strings.Repeat("w", 32))
	t.Setenv("GITHUB_READ_TOKEN", "")
	t.Setenv("BOT_TOKEN", "write-token")
	signer, err := emailReplySigner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	options, err := emailReplyOptions(cfg, signer, []string{"alice"}, "proxy")
	if err != nil {
		t.Fatal(err)
	}
	if options.GenerationToken != "" {
		t.Fatalf("generation token = %q, want empty without GITHUB_READ_TOKEN", options.GenerationToken)
	}
}
