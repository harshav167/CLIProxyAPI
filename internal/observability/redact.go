package observability

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var secretNameFragments = []string{
	"authorization",
	"api-key",
	"apikey",
	"token",
	"secret",
	"password",
	"credential",
	"cookie",
}

func redactHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return map[string]string{}
	}
	redacted := make(map[string]string, len(headers))
	for key, value := range headers {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if isSecretName(key) {
			redacted[key] = "<redacted>"
			continue
		}
		redacted[key] = strings.TrimSpace(value)
	}
	return redacted
}

func safeHeaderValue(key, value string) string {
	if isSecretName(key) {
		return "<redacted>"
	}
	return strings.TrimSpace(value)
}

func isSecretName(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	for _, fragment := range secretNameFragments {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}

func boundedString(value string, max int) string {
	value = strings.TrimSpace(value)
	if max <= 0 || len(value) <= max {
		return value
	}
	return value[:max] + "..."
}

var (
	bearerTokenPattern   = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`)
	commonSecretPatterns = []*regexp.Regexp{
		regexp.MustCompile(`sk-proj-[A-Za-z0-9._~+/=-]{8,}`),
		regexp.MustCompile(`sk-[A-Za-z0-9._~+/=-]{8,}`),
		regexp.MustCompile(`AIza[0-9A-Za-z_-]{20,}`),
		regexp.MustCompile(`sgamp_user_[A-Za-z0-9_=-]{8,}`),
	}
	jsonLikeSecretPatterns = buildJSONLikeSecretPatterns([]string{
		"authorization",
		"api-key",
		"apikey",
		"x-api-key",
		"token",
		"secret",
		"password",
	})
)

func RedactBodyForLog(body []byte, max int) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}
	if redactedJSON, ok := redactJSONBody(text); ok {
		text = redactedJSON
	} else {
		text = redactString(text)
	}
	return boundedString(text, max)
}

func RedactStringForLog(text string, max int) string {
	text = redactString(text)
	return boundedString(text, max)
}

func redactString(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = bearerTokenPattern.ReplaceAllString(text, "Bearer <redacted>")
	for _, pattern := range commonSecretPatterns {
		text = pattern.ReplaceAllString(text, "<redacted>")
	}
	for _, pattern := range jsonLikeSecretPatterns {
		text = pattern.ReplaceAllString(text, "${1}<redacted>${2}")
	}
	return text
}

func redactJSONBody(text string) (string, bool) {
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return "", false
	}
	value = redactJSONValue(value)
	out, err := json.Marshal(value)
	if err != nil {
		return "", false
	}
	return string(out), true
}

func redactJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if isSecretName(key) {
				typed[key] = "<redacted>"
				continue
			}
			typed[key] = redactJSONValue(child)
		}
		return typed
	case []any:
		for index, child := range typed {
			typed[index] = redactJSONValue(child)
		}
		return typed
	case string:
		return redactString(typed)
	default:
		return typed
	}
}

func buildJSONLikeSecretPatterns(names []string) []*regexp.Regexp {
	patterns := make([]*regexp.Regexp, 0, len(names)*3)
	for _, name := range names {
		name = regexp.QuoteMeta(name)
		patterns = append(patterns,
			regexp.MustCompile(fmt.Sprintf(`(?i)(["']?%s["']?\s*[:=]\s*")(?:\\.|[^"\\])*(")`, name)),
			regexp.MustCompile(fmt.Sprintf(`(?i)(["']?%s["']?\s*[:=]\s*')(?:\\.|[^'\\])*(')`, name)),
			regexp.MustCompile(fmt.Sprintf(`(?i)(["']?%s["']?\s*[:=]\s*)[^"',}\s]+()`, name)),
		)
	}
	return patterns
}
