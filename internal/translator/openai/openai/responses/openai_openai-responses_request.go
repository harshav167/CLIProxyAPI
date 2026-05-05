package responses

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertOpenAIResponsesRequestToOpenAIChatCompletions converts OpenAI responses format to OpenAI chat completions format.
// It transforms the OpenAI responses API format (with instructions and input array) into the standard
// OpenAI chat completions format (with messages array and system content).
//
// The conversion handles:
// 1. Model name and streaming configuration
// 2. Instructions to system message conversion
// 3. Input array to messages array transformation
// 4. Tool definitions and tool choice conversion
// 5. Function calls and function results handling
// 6. Generation parameters mapping (max_tokens, reasoning, etc.)
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data in OpenAI responses format
//   - stream: A boolean indicating if the request is for a streaming response
//
// Returns:
//   - []byte: The transformed request data in OpenAI chat completions format
func ConvertOpenAIResponsesRequestToOpenAIChatCompletions(modelName string, inputRawJSON []byte, stream bool) []byte {
	rawJSON := inputRawJSON
	// Base OpenAI chat completions template with default values
	out := []byte(`{"model":"","messages":[],"stream":false}`)

	root := gjson.ParseBytes(rawJSON)

	// Set model name
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Set stream configuration
	out, _ = sjson.SetBytes(out, "stream", stream)

	// Map generation parameters from responses format to chat completions format
	if maxTokens := root.Get("max_output_tokens"); maxTokens.Exists() {
		out, _ = sjson.SetBytes(out, "max_tokens", maxTokens.Int())
	}

	if parallelToolCalls := root.Get("parallel_tool_calls"); parallelToolCalls.Exists() {
		out, _ = sjson.SetBytes(out, "parallel_tool_calls", parallelToolCalls.Bool())
	}

	// Convert instructions to system message
	if instructions := root.Get("instructions"); instructions.Exists() {
		systemMessage := []byte(`{"role":"system","content":""}`)
		systemMessage, _ = sjson.SetBytes(systemMessage, "content", instructions.String())
		out, _ = sjson.SetRawBytes(out, "messages.-1", systemMessage)
	}

	// Convert input array to messages
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			itemType := item.Get("type").String()
			if itemType == "" && item.Get("role").String() != "" {
				itemType = "message"
			}

			switch itemType {
			case "message", "":
				// Handle regular message conversion
				role := item.Get("role").String()
				if role == "developer" {
					role = "user"
				}
				message := []byte(`{"role":"","content":[]}`)
				message, _ = sjson.SetBytes(message, "role", role)

				if content := item.Get("content"); content.Exists() && content.IsArray() {
					var messageContent string
					var toolCalls []interface{}

					content.ForEach(func(_, contentItem gjson.Result) bool {
						contentType := contentItem.Get("type").String()
						if contentType == "" {
							contentType = "input_text"
						}

						switch contentType {
						case "input_text", "output_text":
							text := contentItem.Get("text").String()
							contentPart := []byte(`{"type":"text","text":""}`)
							contentPart, _ = sjson.SetBytes(contentPart, "text", text)
							message, _ = sjson.SetRawBytes(message, "content.-1", contentPart)
						case "input_image":
							imageURL := contentItem.Get("image_url").String()
							contentPart := []byte(`{"type":"image_url","image_url":{"url":""}}`)
							contentPart, _ = sjson.SetBytes(contentPart, "image_url.url", imageURL)
							message, _ = sjson.SetRawBytes(message, "content.-1", contentPart)
						}
						return true
					})

					if messageContent != "" {
						message, _ = sjson.SetBytes(message, "content", messageContent)
					}

					if len(toolCalls) > 0 {
						message, _ = sjson.SetBytes(message, "tool_calls", toolCalls)
					}
				} else if content.Type == gjson.String {
					message, _ = sjson.SetBytes(message, "content", content.String())
				}

				out, _ = sjson.SetRawBytes(out, "messages.-1", message)

			case "function_call", "custom_tool_call":
				// Both function_call (regular OpenAI function tools) AND
				// custom_tool_call (Responses-API custom-grammar tools like
				// ApplyPatch) become an assistant message with tool_calls in
				// chat-completions shape. custom_tool_call uses `input` instead
				// of `arguments` for the tool body — handle both.
				assistantMessage := []byte(`{"role":"assistant","tool_calls":[]}`)

				toolCall := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)

				if callId := item.Get("call_id"); callId.Exists() {
					toolCall, _ = sjson.SetBytes(toolCall, "id", callId.String())
				}

				if name := item.Get("name"); name.Exists() {
					toolCall, _ = sjson.SetBytes(toolCall, "function.name", name.String())
				}

				args := ""
				if v := item.Get("arguments"); v.Exists() {
					args = v.String()
				} else if v := item.Get("input"); v.Exists() {
					args = v.String()
				}
				toolCall, _ = sjson.SetBytes(toolCall, "function.arguments", args)

				assistantMessage, _ = sjson.SetRawBytes(assistantMessage, "tool_calls.0", toolCall)
				out, _ = sjson.SetRawBytes(out, "messages.-1", assistantMessage)

			case "function_call_output", "custom_tool_call_output":
				// Tool result messages — both standard function tools AND custom
				// tools (e.g. ApplyPatch error messages like "Failed to find
				// context"). Without handling custom_tool_call_output, the
				// model never sees ApplyPatch failure messages and loops trying
				// the same patch repeatedly.
				toolMessage := []byte(`{"role":"tool","tool_call_id":"","content":""}`)

				if callId := item.Get("call_id"); callId.Exists() {
					toolMessage, _ = sjson.SetBytes(toolMessage, "tool_call_id", callId.String())
				}

				if output := item.Get("output"); output.Exists() {
					// custom_tool_call_output emits output as an array of
					// {type:"input_text", text:"..."} blocks. function_call_output
					// emits output as a plain string. Handle both: concatenate
					// text from blocks if it's an array; otherwise treat as string.
					var content string
					if output.IsArray() {
						var parts []string
						output.ForEach(func(_, blk gjson.Result) bool {
							if t := blk.Get("text"); t.Exists() && t.Type == gjson.String {
								parts = append(parts, t.String())
							}
							return true
						})
						content = strings.Join(parts, "\n")
					} else {
						content = output.String()
					}
					toolMessage, _ = sjson.SetBytes(toolMessage, "content", content)
				}

				out, _ = sjson.SetRawBytes(out, "messages.-1", toolMessage)
			}

			return true
		})
	} else if input.Type == gjson.String {
		msg := []byte(`{}`)
		msg, _ = sjson.SetBytes(msg, "role", "user")
		msg, _ = sjson.SetBytes(msg, "content", input.String())
		out, _ = sjson.SetRawBytes(out, "messages.-1", msg)
	}

	// Convert tools from responses format to chat completions format
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		var chatCompletionsTools []interface{}

		tools.ForEach(func(_, tool gjson.Result) bool {
			// Built-in tools (e.g. {"type":"web_search"}, {"type":"apply_patch"},
			// {"type":"local_shell"}) MUST pass through verbatim. Cursor BYOK
			// (native GPT mode) and other clients rely on these reaching the
			// upstream Codex /responses endpoint, which is the canonical OpenAI
			// API surface that supports them. Previously this branch silently
			// dropped them — that broke apply_patch and friends entirely.
			toolType := tool.Get("type").String()
			if toolType != "" && toolType != "function" && tool.IsObject() {
				chatCompletionsTools = append(chatCompletionsTools, gjson.ParseBytes([]byte(tool.Raw)).Value())
				return true
			}

			chatTool := []byte(`{"type":"function","function":{}}`)

			// Convert tool structure from responses format to chat completions format
			function := []byte(`{"name":"","description":"","parameters":{}}`)

			if name := tool.Get("name"); name.Exists() {
				function, _ = sjson.SetBytes(function, "name", name.String())
			}

			if description := tool.Get("description"); description.Exists() {
				function, _ = sjson.SetBytes(function, "description", description.String())
			}

			if parameters := tool.Get("parameters"); parameters.Exists() {
				function, _ = sjson.SetRawBytes(function, "parameters", []byte(parameters.Raw))
			}

			chatTool, _ = sjson.SetRawBytes(chatTool, "function", function)
			chatCompletionsTools = append(chatCompletionsTools, gjson.ParseBytes(chatTool).Value())

			return true
		})

		if len(chatCompletionsTools) > 0 {
			out, _ = sjson.SetBytes(out, "tools", chatCompletionsTools)
		}
	}

	if reasoningEffort := root.Get("reasoning.effort"); reasoningEffort.Exists() {
		effort := strings.ToLower(strings.TrimSpace(reasoningEffort.String()))
		if effort != "" {
			out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
		}
	}

	// Convert tool_choice if present
	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() {
		out, _ = sjson.SetBytes(out, "tool_choice", toolChoice.String())
	}

	// Preserve prompt_cache_key through the responses→chat conversion so
	// chat→codex can carry it upstream; otherwise Cursor BYOK falls back
	// to content-prefix matching and loses warm-turn cache locality.
	if v := root.Get("prompt_cache_key"); v.Exists() && v.String() != "" {
		out, _ = sjson.SetBytes(out, "prompt_cache_key", v.String())
	}
	if v := root.Get("safety_identifier"); v.Exists() && v.String() != "" {
		out, _ = sjson.SetBytes(out, "safety_identifier", v.String())
	}
	if v := root.Get("user"); v.Exists() && v.String() != "" {
		out, _ = sjson.SetBytes(out, "user", v.String())
	}
	// Preserve service_tier through responses→chat conversion. Cursor's
	// "Fast" mode toggle on Responses-shape input[] bodies (gpt-5.4/5.5
	// BYOK path) sends `service_tier: "priority"` here. Without this
	// passthrough the field is dropped at the very first hop of the
	// Cursor→cli-proxy→Codex chain, so the downstream chat→codex
	// translator's preserve-priority logic has nothing to forward.
	// Forwarding only "priority" matches the codex/responses translator
	// policy (which strips other tier values that Codex /responses rejects).
	if v := root.Get("service_tier"); v.Exists() && v.String() == "priority" {
		out, _ = sjson.SetBytes(out, "service_tier", "priority")
	}

	return out
}
