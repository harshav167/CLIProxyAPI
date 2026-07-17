package kimi

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type modelWireThinking int

const (
	modelWireThinkingConfigurable modelWireThinking = iota
	modelWireThinkingK3Max
	modelWireThinkingAlwaysEnabled
)

func classifyModelWireThinking(model string) modelWireThinking {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "k3":
		return modelWireThinkingK3Max
	case "kimi-for-coding", "kimi-for-coding-highspeed":
		return modelWireThinkingAlwaysEnabled
	default:
		return modelWireThinkingConfigurable
	}
}

// EnforceModelWireThinking reapplies the fixed Kimi wire shape for models whose
// upstream protocol ignores or rejects caller-selected thinking fields.
func EnforceModelWireThinking(body []byte, model string) ([]byte, error) {
	switch classifyModelWireThinking(model) {
	case modelWireThinkingK3Max:
		return applyK3WireThinking(body)
	case modelWireThinkingAlwaysEnabled:
		return applyEnabledWireThinking(body)
	default:
		return body, nil
	}
}

func validKimiBody(body []byte) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return []byte(`{}`)
	}
	return body
}

func applyK3WireThinking(body []byte) ([]byte, error) {
	body = validKimiBody(body)
	result, errDeleteThinking := sjson.DeleteBytes(body, "thinking")
	if errDeleteThinking != nil {
		return body, fmt.Errorf("kimi thinking: failed to clear K3 thinking object: %w", errDeleteThinking)
	}
	result, errSetEffort := sjson.SetBytes(result, "reasoning_effort", "max")
	if errSetEffort != nil {
		return body, fmt.Errorf("kimi thinking: failed to set K3 reasoning_effort: %w", errSetEffort)
	}
	return result, nil
}

func applyEnabledWireThinking(body []byte) ([]byte, error) {
	body = validKimiBody(body)
	result, errDeleteEffort := sjson.DeleteBytes(body, "reasoning_effort")
	if errDeleteEffort != nil {
		return body, fmt.Errorf("kimi thinking: failed to clear reasoning_effort: %w", errDeleteEffort)
	}
	result, errSetType := sjson.SetBytes(result, "thinking.type", "enabled")
	if errSetType != nil {
		return body, fmt.Errorf("kimi thinking: failed to set thinking.type: %w", errSetType)
	}
	return result, nil
}
