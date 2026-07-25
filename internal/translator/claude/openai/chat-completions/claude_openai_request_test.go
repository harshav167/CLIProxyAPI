package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToClaude_SanitizesToolCallIDsForClaude(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"messages": [
			{
				"role": "assistant",
				"tool_calls": [
					{
						"id": "call.with space:1",
						"type": "function",
						"function": {
							"name": "Read",
							"arguments": "{\"path\":\"README.md\"}"
						}
					}
				]
			},
			{
				"role": "tool",
				"tool_call_id": "call.with space:1",
				"content": "ok"
			}
		]
	}`

	result := ConvertOpenAIRequestToClaude("claude-sonnet-4-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	toolUseID := resultJSON.Get("messages.0.content.0.id").String()
	toolResultID := resultJSON.Get("messages.1.content.0.tool_use_id").String()

	if toolUseID != "call_with_space_1" {
		t.Fatalf("tool_use id = %q, want %q", toolUseID, "call_with_space_1")
	}
	if toolResultID != toolUseID {
		t.Fatalf("tool_result tool_use_id = %q, want same sanitized id %q", toolResultID, toolUseID)
	}
}

func TestConvertOpenAIRequestToClaude_DropsTemperature(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"temperature": 0.2,
		"top_p": 0.8,
		"messages": [
			{"role": "user", "content": "hi"}
		]
	}`

	result := ConvertOpenAIRequestToClaude("claude-sonnet-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)

	if resultJSON.Get("temperature").Exists() {
		t.Fatalf("temperature should be removed")
	}
	if got := resultJSON.Get("top_p").Float(); got != 0.8 {
		t.Fatalf("top_p = %v, want 0.8", got)
	}
}

func TestConvertOpenAIRequestToClaude_ToolResultTextAndBase64Image(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"messages": [
			{
				"role": "assistant",
				"content": "",
				"tool_calls": [
					{
						"id": "call_1",
						"type": "function",
						"function": {
							"name": "do_work",
							"arguments": "{\"a\":1}"
						}
					}
				]
			},
			{
				"role": "tool",
				"tool_call_id": "call_1",
				"content": [
					{"type": "text", "text": "tool ok"},
					{
						"type": "image_url",
						"image_url": {
							"url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="
						}
					}
				]
			}
		]
	}`

	result := ConvertOpenAIRequestToClaude("claude-sonnet-4-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	messages := resultJSON.Get("messages").Array()

	if len(messages) != 2 {
		t.Fatalf("Expected 2 messages, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}

	toolResult := messages[1].Get("content.0")
	if got := toolResult.Get("type").String(); got != "tool_result" {
		t.Fatalf("Expected content[0].type %q, got %q", "tool_result", got)
	}
	if got := toolResult.Get("tool_use_id").String(); got != "call_1" {
		t.Fatalf("Expected tool_use_id %q, got %q", "call_1", got)
	}

	toolContent := toolResult.Get("content")
	if !toolContent.IsArray() {
		t.Fatalf("Expected tool_result content array, got %s", toolContent.Raw)
	}
	if got := toolContent.Get("0.type").String(); got != "text" {
		t.Fatalf("Expected first tool_result part type %q, got %q", "text", got)
	}
	if got := toolContent.Get("0.text").String(); got != "tool ok" {
		t.Fatalf("Expected first tool_result part text %q, got %q", "tool ok", got)
	}
	if got := toolContent.Get("1.type").String(); got != "image" {
		t.Fatalf("Expected second tool_result part type %q, got %q", "image", got)
	}
	if got := toolContent.Get("1.source.type").String(); got != "base64" {
		t.Fatalf("Expected image source type %q, got %q", "base64", got)
	}
	if got := toolContent.Get("1.source.media_type").String(); got != "image/png" {
		t.Fatalf("Expected image media type %q, got %q", "image/png", got)
	}
	if got := toolContent.Get("1.source.data").String(); got != "iVBORw0KGgoAAAANSUhEUg==" {
		t.Fatalf("Unexpected base64 image data: %q", got)
	}
}

func TestConvertOpenAIRequestToClaude_ToolResultURLImageOnly(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"messages": [
			{
				"role": "assistant",
				"content": "",
				"tool_calls": [
					{
						"id": "call_1",
						"type": "function",
						"function": {
							"name": "do_work",
							"arguments": "{\"a\":1}"
						}
					}
				]
			},
			{
				"role": "tool",
				"tool_call_id": "call_1",
				"content": [
					{
						"type": "image_url",
						"image_url": {
							"url": "https://example.com/tool.png"
						}
					}
				]
			}
		]
	}`

	result := ConvertOpenAIRequestToClaude("claude-sonnet-4-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	messages := resultJSON.Get("messages").Array()

	if len(messages) != 2 {
		t.Fatalf("Expected 2 messages, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}

	toolContent := messages[1].Get("content.0.content")
	if !toolContent.IsArray() {
		t.Fatalf("Expected tool_result content array, got %s", toolContent.Raw)
	}
	if got := toolContent.Get("0.type").String(); got != "image" {
		t.Fatalf("Expected tool_result part type %q, got %q", "image", got)
	}
	if got := toolContent.Get("0.source.type").String(); got != "url" {
		t.Fatalf("Expected image source type %q, got %q", "url", got)
	}
	if got := toolContent.Get("0.source.url").String(); got != "https://example.com/tool.png" {
		t.Fatalf("Unexpected image URL: %q", got)
	}
}

func TestConvertOpenAIRequestToClaude_SystemRoleBecomesTopLevelSystem(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"messages": [
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": "Hello"}
		]
	}`

	result := ConvertOpenAIRequestToClaude("claude-sonnet-4-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)

	system := resultJSON.Get("system")
	if !system.IsArray() {
		t.Fatalf("Expected top-level system array, got %s", system.Raw)
	}
	if len(system.Array()) != 1 {
		t.Fatalf("Expected 1 system block, got %d. System: %s", len(system.Array()), system.Raw)
	}
	if got := system.Get("0.type").String(); got != "text" {
		t.Fatalf("Expected system block type %q, got %q", "text", got)
	}
	if got := system.Get("0.text").String(); got != "You are a helpful assistant." {
		t.Fatalf("Expected system text %q, got %q", "You are a helpful assistant.", got)
	}

	messages := resultJSON.Get("messages").Array()
	if len(messages) != 1 {
		t.Fatalf("Expected 1 non-system message, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}
	if got := messages[0].Get("role").String(); got != "user" {
		t.Fatalf("Expected remaining message role %q, got %q", "user", got)
	}
	if got := messages[0].Get("content.0.text").String(); got != "Hello" {
		t.Fatalf("Expected user text %q, got %q", "Hello", got)
	}
}

func TestConvertOpenAIRequestToClaude_MultipleSystemMessagesMergedIntoTopLevelSystem(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"messages": [
			{"role": "system", "content": "Rule 1"},
			{"role": "system", "content": [{"type": "text", "text": "Rule 2"}]},
			{"role": "user", "content": "Hello"}
		]
	}`

	result := ConvertOpenAIRequestToClaude("claude-sonnet-4-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)

	system := resultJSON.Get("system").Array()
	if len(system) != 2 {
		t.Fatalf("Expected 2 system blocks, got %d. System: %s", len(system), resultJSON.Get("system").Raw)
	}
	if got := system[0].Get("text").String(); got != "Rule 1" {
		t.Fatalf("Expected first system text %q, got %q", "Rule 1", got)
	}
	if got := system[1].Get("text").String(); got != "Rule 2" {
		t.Fatalf("Expected second system text %q, got %q", "Rule 2", got)
	}

	messages := resultJSON.Get("messages").Array()
	if len(messages) != 1 {
		t.Fatalf("Expected 1 non-system message, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}
	if got := messages[0].Get("role").String(); got != "user" {
		t.Fatalf("Expected remaining message role %q, got %q", "user", got)
	}
	if got := messages[0].Get("content.0.text").String(); got != "Hello" {
		t.Fatalf("Expected user text %q, got %q", "Hello", got)
	}
}

func TestConvertOpenAIRequestToClaude_SystemOnlyInputKeepsFallbackUserMessage(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"messages": [
			{"role": "system", "content": "You are a helpful assistant."}
		]
	}`

	result := ConvertOpenAIRequestToClaude("claude-sonnet-4-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)

	system := resultJSON.Get("system").Array()
	if len(system) != 1 {
		t.Fatalf("Expected 1 system block, got %d. System: %s", len(system), resultJSON.Get("system").Raw)
	}
	if got := system[0].Get("text").String(); got != "You are a helpful assistant." {
		t.Fatalf("Expected system text %q, got %q", "You are a helpful assistant.", got)
	}

	messages := resultJSON.Get("messages").Array()
	if len(messages) != 1 {
		t.Fatalf("Expected 1 fallback message, got %d. Messages: %s", len(messages), resultJSON.Get("messages").Raw)
	}
	if got := messages[0].Get("role").String(); got != "user" {
		t.Fatalf("Expected fallback message role %q, got %q", "user", got)
	}
	if got := messages[0].Get("content.0.type").String(); got != "text" {
		t.Fatalf("Expected fallback content type %q, got %q", "text", got)
	}
	if got := messages[0].Get("content.0.text").String(); got != "" {
		t.Fatalf("Expected fallback text %q, got %q", "", got)
	}
}

func TestConvertOpenAIRequestToClaude_OpusThinkingAliasesUseClaudeCodeParityShape(t *testing.T) {
	inputJSON := `{
		"messages": [{"role": "user", "content": "hi"}],
		"stream": true
	}`

	for _, candidate := range []string{"claude-opus-4-7", "claude-opus-4-8", "claude-opus-5"} {
		t.Run(candidate, func(t *testing.T) {
			assertClaudeCodeAdaptiveThinkingAliases(t, candidate, inputJSON)
		})
	}
}

func assertClaudeCodeAdaptiveThinkingAliases(t *testing.T, modelFamily, inputJSON string) {
	t.Helper()

	low := gjson.ParseBytes(ConvertOpenAIRequestToClaude(modelFamily+"-thinking-low", []byte(inputJSON), true))
	medium := gjson.ParseBytes(ConvertOpenAIRequestToClaude(modelFamily+"-thinking-medium", []byte(inputJSON), true))
	high := gjson.ParseBytes(ConvertOpenAIRequestToClaude(modelFamily+"-thinking-high", []byte(inputJSON), true))
	xhigh := gjson.ParseBytes(ConvertOpenAIRequestToClaude(modelFamily+"-thinking-xhigh", []byte(inputJSON), true))
	max := gjson.ParseBytes(ConvertOpenAIRequestToClaude(modelFamily+"-thinking-max", []byte(inputJSON), true))

	for name, resultJSON := range map[string]gjson.Result{"low": low, "medium": medium, "high": high, "xhigh": xhigh, "max": max} {
		if got := resultJSON.Get("thinking.type").String(); got != "adaptive" {
			t.Fatalf("%s thinking.type = %q, want adaptive; body=%s", name, got, resultJSON.Raw)
		}
		if got := resultJSON.Get("thinking.display"); got.Exists() {
			t.Fatalf("%s thinking.display should be omitted for adaptive Opus thinking, got %s; body=%s", name, got.Raw, resultJSON.Raw)
		}
		if got := resultJSON.Get("max_tokens").Int(); got != 64000 {
			t.Fatalf("%s max_tokens = %d, want 64000; body=%s", name, got, resultJSON.Raw)
		}
		if got := resultJSON.Get("thinking.budget_tokens"); got.Exists() {
			t.Fatalf("%s thinking.budget_tokens should be omitted for adaptive Opus thinking, got %s; body=%s", name, got.Raw, resultJSON.Raw)
		}
		if got := resultJSON.Get("context_management.edits.0.type").String(); got != "clear_thinking_20251015" {
			t.Fatalf("%s context_management edit type = %q, want clear_thinking_20251015; body=%s", name, got, resultJSON.Raw)
		}
		if got := resultJSON.Get("context_management.edits.0.keep").String(); got != "all" {
			t.Fatalf("%s context_management edit keep = %q, want all; body=%s", name, got, resultJSON.Raw)
		}
		if got := resultJSON.Get("diagnostics"); got.Exists() {
			t.Fatalf("%s translator must leave diagnostics to the executor session store, got %s; body=%s", name, got.Raw, resultJSON.Raw)
		}
	}
	if got := low.Get("output_config.effort").String(); got != "low" {
		t.Fatalf("low effort = %q, want low; body=%s", got, low.Raw)
	}
	if got := medium.Get("output_config.effort").String(); got != "medium" {
		t.Fatalf("medium effort = %q, want medium; body=%s", got, medium.Raw)
	}
	if got := high.Get("output_config.effort").String(); got != "high" {
		t.Fatalf("high effort = %q, want high; body=%s", got, high.Raw)
	}
	if got := xhigh.Get("output_config.effort").String(); got != "xhigh" {
		t.Fatalf("xhigh effort = %q, want xhigh; body=%s", got, xhigh.Raw)
	}
	if got := max.Get("output_config.effort").String(); got != "max" {
		t.Fatalf("max effort = %q, want max; body=%s", got, max.Raw)
	}
}

func TestConvertOpenAIRequestToClaude_OpusThinkingPreservesIncomingEffort(t *testing.T) {
	inputJSON := `{
		"messages": [{"role": "user", "content": "hi"}],
		"thinking": {"type": "enabled", "budget_tokens": 2048},
		"output_config": {"effort": "medium"}
	}`

	resultJSON := gjson.ParseBytes(ConvertOpenAIRequestToClaude("claude-4.6-opus-high-thinking", []byte(inputJSON), true))
	if got := resultJSON.Get("output_config.effort").String(); got != "medium" {
		t.Fatalf("effort = %q, want preserved medium; body=%s", got, resultJSON.Raw)
	}
	if got := resultJSON.Get("thinking.display").String(); got != "summarized" {
		t.Fatalf("thinking.display = %q, want summarized; body=%s", got, resultJSON.Raw)
	}
	if got := resultJSON.Get("thinking.budget_tokens").Int(); got != 63999 {
		t.Fatalf("thinking.budget_tokens = %d, want parity budget 63999; body=%s", got, resultJSON.Raw)
	}
}

func TestConvertOpenAIRequestToClaude_OpusThinkingSkippedForForcedToolChoice(t *testing.T) {
	// Anthropic rejects extended thinking combined with a forced tool_choice.
	// The thinking-parity path must skip injecting thinking for ALL forced
	// forms, including the OpenAI dialects that are only converted to Anthropic
	// any/tool later in the same function.
	cases := []struct {
		name       string
		toolChoice string
	}{
		{"openai_required_string", `"required"`},
		{"openai_function_object", `{"type":"function","function":{"name":"do_work"}}`},
		{"anthropic_any", `{"type":"any"}`},
		{"anthropic_tool", `{"type":"tool","name":"do_work"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inputJSON := `{
				"messages": [{"role": "user", "content": "hi"}],
				"tools": [{"type":"function","function":{"name":"do_work","parameters":{"type":"object","properties":{}}}}],
				"tool_choice": ` + tc.toolChoice + `,
				"output_config": {"effort": "high"}
			}`
			resultJSON := gjson.ParseBytes(ConvertOpenAIRequestToClaude("claude-opus-4-8-thinking-high", []byte(inputJSON), true))

			// Parity must NOT have injected extended thinking.
			if got := resultJSON.Get("thinking.type").String(); got == "enabled" || got == "adaptive" {
				t.Fatalf("thinking.type = %q, want unset (forced tool choice should suppress thinking); body=%s", got, resultJSON.Raw)
			}
			if resultJSON.Get("thinking.budget_tokens").Exists() {
				t.Fatalf("thinking.budget_tokens should not be set with forced tool choice; body=%s", resultJSON.Raw)
			}
			if resultJSON.Get("context_management.edits.0.type").Exists() {
				t.Fatalf("context_management edits should not be set with forced tool choice; body=%s", resultJSON.Raw)
			}
		})
	}
}

func TestConvertOpenAIRequestToClaude_OpusThinkingDisabledDoesNotAddParityFields(t *testing.T) {
	inputJSON := `{
		"messages": [{"role": "user", "content": "hi"}],
		"thinking": {"type": "disabled"},
		"output_config": {"effort": "high"}
	}`

	resultJSON := gjson.ParseBytes(ConvertOpenAIRequestToClaude("claude-opus-4-7-thinking-high", []byte(inputJSON), true))
	if got := resultJSON.Get("thinking.type").String(); got != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled; body=%s", got, resultJSON.Raw)
	}
	if resultJSON.Get("thinking.display").Exists() {
		t.Fatalf("thinking.display should not be added when disabled; body=%s", resultJSON.Raw)
	}
	if got := resultJSON.Get("thinking.budget_tokens"); got.Exists() {
		t.Fatalf("thinking.budget_tokens should not be added when disabled, got %s; body=%s", got.Raw, resultJSON.Raw)
	}
}
