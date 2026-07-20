package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

const (
	evidenceTokenVersion = "v1"
	maxEvidencePathBytes = 4096
)

type evidenceAttestor struct {
	key []byte
}

func newEvidenceAttestor(secret string) *evidenceAttestor {
	sum := sha256.Sum256([]byte("prow-ai-artifact-evidence\x00" + secret))
	return &evidenceAttestor{key: append([]byte(nil), sum[:]...)}
}

func (a *evidenceAttestor) issue(scope, path string) string {
	path = normalizeEvidencePath(path)
	if a == nil || len(a.key) == 0 || path == "" || len(path) > maxEvidencePathBytes {
		return ""
	}
	encodedPath := base64.RawURLEncoding.EncodeToString([]byte(path))
	mac := a.mac(scope, path)
	return strings.Join([]string{evidenceTokenVersion, encodedPath, base64.RawURLEncoding.EncodeToString(mac)}, ".")
}

func (a *evidenceAttestor) verify(scope, token string) (string, bool) {
	if a == nil || len(a.key) == 0 {
		return "", false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != evidenceTokenVersion {
		return "", false
	}
	pathBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(pathBytes) == 0 || len(pathBytes) > maxEvidencePathBytes {
		return "", false
	}
	path := string(pathBytes)
	if normalizeEvidencePath(path) != path {
		return "", false
	}
	provided, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(provided, a.mac(scope, path)) {
		return "", false
	}
	return path, true
}

func (a *evidenceAttestor) mac(scope, path string) []byte {
	mac := hmac.New(sha256.New, a.key)
	_, _ = mac.Write([]byte(evidenceTokenVersion))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(scope))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(path))
	return mac.Sum(nil)
}

func attachEvidenceToken(attestor *evidenceAttestor, scope, toolName string, payload map[string]interface{}) {
	if payload == nil || !isEvidenceTool(toolName) {
		return
	}
	if _, failed := payload["error"]; failed {
		return
	}
	path, _ := payload["path"].(string)
	if token := attestor.issue(scope, path); token != "" {
		payload["evidence_token"] = token
	}
}

func isEvidenceTool(name string) bool {
	switch strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "-", "_") {
	case "read_artifact", "tail_artifact", "grep_artifact":
		return true
	}
	return false
}
