package ai

import "strings"

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
