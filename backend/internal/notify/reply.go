package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

// ReplyKind identifies the resource addressed by an email reply token.
type ReplyKind string

const (
	ReplyPattern ReplyKind = "pattern"
	ReplyRequest ReplyKind = "request"
)

var (
	ErrInvalidReplyToken = errors.New("invalid email reply token")
	ErrExpiredReplyToken = errors.New("email reply token expired")
)

// ReplyTarget is the validated resource encoded in a reply address.
type ReplyTarget struct {
	Kind      ReplyKind
	ID        string
	ExpiresAt time.Time
}

// ReplySigner creates and validates short-lived HMAC-protected reply addresses.
type ReplySigner struct {
	template string
	prefix   string
	suffix   string
	secret   []byte
}

// NewReplySigner validates a reply address template and signing secret.
func NewReplySigner(template, secret string) (*ReplySigner, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("email reply token secret must be at least 32 characters")
	}
	if strings.Count(template, "{token}") != 1 {
		return nil, fmt.Errorf("email reply address must contain exactly one {token} placeholder")
	}
	prefix, suffix, _ := strings.Cut(template, "{token}")
	if strings.Contains(prefix, "@") || !strings.HasPrefix(suffix, "@") {
		return nil, fmt.Errorf("email reply token placeholder must be in the local part")
	}
	sample := strings.Repeat("a", 60)
	rendered := prefix + sample + suffix
	parsed, err := mail.ParseAddress(rendered)
	if err != nil || parsed.Name != "" || parsed.Address != rendered {
		return nil, fmt.Errorf("invalid email reply address template")
	}
	local, _, ok := strings.Cut(parsed.Address, "@")
	if !ok || len(local) > 64 {
		return nil, fmt.Errorf("email reply address local part exceeds 64 characters")
	}
	return &ReplySigner{
		template: template,
		prefix:   strings.ToLower(prefix),
		suffix:   strings.ToLower(suffix),
		secret:   []byte(secret),
	}, nil
}

// Address returns a signed Reply-To address for one pattern or action request.
func (s *ReplySigner) Address(kind ReplyKind, id string, expiresAt time.Time) (mail.Address, error) {
	marker, wantLen, err := replyKindShape(kind)
	if err != nil || !validReplyID(id, wantLen) {
		return mail.Address{}, ErrInvalidReplyToken
	}
	if expiresAt.IsZero() {
		return mail.Address{}, fmt.Errorf("email reply token expiry is required")
	}
	payload := marker + "." + strings.ToLower(id) + "." + strconv.FormatInt(expiresAt.UTC().Unix(), 16)
	token := payload + "." + s.sign(payload)
	rendered := strings.Replace(s.template, "{token}", token, 1)
	parsed, err := mail.ParseAddress(rendered)
	if err != nil {
		return mail.Address{}, fmt.Errorf("rendering email reply address: %w", err)
	}
	if parsed.Name != "" {
		return mail.Address{}, fmt.Errorf("rendering email reply address: display names are not supported")
	}
	local, _, ok := strings.Cut(parsed.Address, "@")
	if !ok || len(local) > 64 {
		return mail.Address{}, fmt.Errorf("rendering email reply address: local part exceeds 64 characters")
	}
	return *parsed, nil
}

// ParseRecipient validates a signed inbound recipient address.
func (s *ReplySigner) ParseRecipient(recipient string, now time.Time) (ReplyTarget, error) {
	parsed, err := mail.ParseAddress(recipient)
	if err != nil {
		return ReplyTarget{}, ErrInvalidReplyToken
	}
	address := strings.ToLower(parsed.Address)
	if !strings.HasPrefix(address, s.prefix) || !strings.HasSuffix(address, s.suffix) {
		return ReplyTarget{}, ErrInvalidReplyToken
	}
	end := len(address) - len(s.suffix)
	if end < len(s.prefix) {
		return ReplyTarget{}, ErrInvalidReplyToken
	}
	token := address[len(s.prefix):end]
	parts := strings.Split(token, ".")
	if len(parts) != 4 {
		return ReplyTarget{}, ErrInvalidReplyToken
	}
	kind, wantLen, err := parseReplyMarker(parts[0])
	if err != nil || !validReplyID(parts[1], wantLen) {
		return ReplyTarget{}, ErrInvalidReplyToken
	}
	expiresUnix, err := strconv.ParseInt(parts[2], 16, 64)
	if err != nil || expiresUnix <= 0 {
		return ReplyTarget{}, ErrInvalidReplyToken
	}
	payload := strings.Join(parts[:3], ".")
	wantSignature := s.sign(payload)
	if !hmac.Equal([]byte(parts[3]), []byte(wantSignature)) {
		return ReplyTarget{}, ErrInvalidReplyToken
	}
	expiresAt := time.Unix(expiresUnix, 0).UTC()
	if !now.UTC().Before(expiresAt) {
		return ReplyTarget{}, ErrExpiredReplyToken
	}
	return ReplyTarget{Kind: kind, ID: parts[1], ExpiresAt: expiresAt}, nil
}

func (s *ReplySigner) sign(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(payload))
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(mac.Sum(nil)[:10]))
}

func replyKindShape(kind ReplyKind) (string, int, error) {
	switch kind {
	case ReplyPattern:
		return "p", 16, nil
	case ReplyRequest:
		return "r", 32, nil
	default:
		return "", 0, ErrInvalidReplyToken
	}
}

func parseReplyMarker(marker string) (ReplyKind, int, error) {
	switch marker {
	case "p":
		return ReplyPattern, 16, nil
	case "r":
		return ReplyRequest, 32, nil
	default:
		return "", 0, ErrInvalidReplyToken
	}
}

func validReplyID(id string, length int) bool {
	if len(id) != length {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
