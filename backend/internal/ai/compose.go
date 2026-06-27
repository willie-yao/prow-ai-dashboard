package ai

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// PromptFingerprint returns a stable short hex fingerprint of the composed
// system prompt. It is stamped onto every analysis so cache reads can reject
// entries produced by a different prompt. The fingerprint covers the engine
// base, consumer addendum, and response-format footer, so prompt edits
// re-analyze affected failures without a manual cache clear.
func PromptFingerprint(composedPrompt string) string {
	sum := sha256.Sum256([]byte(composedPrompt))
	return hex.EncodeToString(sum[:8])
}

// ComposeSystemPrompt assembles the final system prompt sent to the model on
// every chat completion. The shape is fixed:
//
//	<engine BasePrompt>
//
//	## Project-specific knowledge
//
//	<consumer addendum, verbatim>
//
//	<engine ResponseFormatFooter>
//
// The consumer addendum is mandatory; cmd/fetcher hard-errors at startup if
// the consumer's prompts/system.md is missing or whitespace-only.
func ComposeSystemPrompt(consumerAddendum string) string {
	var b strings.Builder
	b.WriteString(BasePrompt)
	b.WriteString("\n\n## Project-specific knowledge\n\n")
	b.WriteString(strings.TrimSpace(consumerAddendum))
	b.WriteString("\n\n")
	b.WriteString(ResponseFormatFooter)
	return b.String()
}
