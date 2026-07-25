package registry

import "strings"

// IsAdaptiveClaudeOpusModel reports whether model is a supported adaptive
// Claude Opus family or one of its thinking-level aliases.
func IsAdaptiveClaudeOpusModel(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	for _, family := range []string{"claude-opus-4-7", "claude-opus-4-8", "claude-opus-5"} {
		if name == family || strings.HasPrefix(name, family+"-thinking-") {
			return true
		}
	}
	return false
}
