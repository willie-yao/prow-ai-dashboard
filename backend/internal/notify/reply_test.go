package notify

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReplySignerRoundTrip(t *testing.T) {
	signer, err := NewReplySigner("{token}@replies.example.com", strings.Repeat("s", 32))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	for _, tc := range []struct {
		kind ReplyKind
		id   string
	}{
		{ReplyPattern, "0123456789abcdef"},
		{ReplyRequest, "0123456789abcdef0123456789abcdef"},
	} {
		address, err := signer.Address(tc.kind, tc.id, now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		local, _, _ := strings.Cut(address.Address, "@")
		if len(local) > 64 {
			t.Fatalf("local part length = %d", len(local))
		}
		target, err := signer.ParseRecipient(address.String(), now)
		if err != nil || target.Kind != tc.kind || target.ID != tc.id || !target.ExpiresAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("target=%+v err=%v", target, err)
		}
	}
}

func TestReplySignerRejectsTamperingAndExpiry(t *testing.T) {
	signer, err := NewReplySigner("{token}@replies.example.com", strings.Repeat("s", 32))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	address, err := signer.Address(ReplyPattern, "0123456789abcdef", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(address.Address, "0123456789abcdef", "1123456789abcdef", 1)
	if _, err := signer.ParseRecipient(tampered, now); !errors.Is(err, ErrInvalidReplyToken) {
		t.Fatalf("tampered err=%v", err)
	}
	if _, err := signer.ParseRecipient(address.Address, now.Add(2*time.Hour)); !errors.Is(err, ErrExpiredReplyToken) {
		t.Fatalf("expired err=%v", err)
	}
}

func TestNewReplySignerValidation(t *testing.T) {
	for _, tc := range []struct {
		name, template, secret string
	}{
		{"short secret", "{token}@example.com", "short"},
		{"missing token", "replies@example.com", strings.Repeat("s", 32)},
		{"token in domain", "replies@{token}.example.com", strings.Repeat("s", 32)},
		{"long local part", "prefix-{token}@example.com", strings.Repeat("s", 32)},
		{"display name", "Replies <{token}@example.com>", strings.Repeat("s", 32)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewReplySigner(tc.template, tc.secret); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
