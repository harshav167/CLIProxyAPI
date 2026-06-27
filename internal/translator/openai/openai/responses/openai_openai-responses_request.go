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
		inputItems := input.Array()
		// Prepass: collect the call IDs that have a tool output later in the
		// input. The adjacency logic (appendRegularMessage/hasAwaitingToolOutput)
		// uses this to know whether a tool call's output is actually present, so
		// it can defer intervening regular messages until after the output and
		// keep the assistant(tool_calls) -> tool(tool_call_id) pair contiguous.
		//
		// MUST track BOTH function_call_output AND custom_tool_call_output. The
		// switch below handles both output types; if the prepass only recorded
		// function_call_output, a custom_tool_call followed by a regular message
		// then its custom_tool_call_output would not be seen as "awaiting",
		// hasAwaitingToolOutput() returns false, the message is NOT deferred, and
		// it gets inserted between the assistant tool_calls and the tool output —
		// invalid Chat Completions ordering.
		outputCallIDs := make(map[string]struct{})
		for _, item := range inputItems {
			itemType := item.Get("type").String()
			if itemType != "function_call_output" && itemType != "custom_tool_call_output" {
				continue
			}
			callID := strings.TrimSpace(item.Get("call_id").String())
			if callID == "" {
				continue
			}
			outputCallIDs[callID] = struct{}{}
		}

		pendingToolCalls := make([]interface{}, 0)
		pendingToolCallIDs := make([]string, 0)
		pendingReasoningContent := ""
		awaitingToolOutputs := make(map[string]struct{})
		deferredMessages := make([][]byte, 0)

		takePendingReasoningContent := func() string {
			reasoningContent := pendingReasoningContent
			pendingReasoningContent = ""
			return reasoningContent
		}
		flushPendingToolCalls := func() {
			if len(pendingToolCalls) == 0 {
				return
			}
			assistantMessage := []byte(`{"role":"assistant","tool_calls":[]}`)
			assistantMessage, _ = sjson.SetBytes(assistantMessage, "tool_calls", pendingToolCalls)
			if reasoningContent := takePendingReasoningContent(); reasoningContent != "" {
				assistantMessage, _ = sjson.SetBytes(assistantMessage, "reasoning_content", reasoningContent)
			}
			out, _ = sjson.SetRawBytes(out, "messages.-1", assistantMessage)
			for _, id := range pendingToolCallIDs {
				if strings.TrimSpace(id) == "" {
					continue
				}
				awaitingToolOutputs[id] = struct{}{}
			}
			pendingToolCalls = pendingToolCalls[:0]
			pendingToolCallIDs = pendingToolCallIDs[:0]
		}
		flushDeferredMessages := func() {
			for _, message := range deferredMessages {
				out, _ = sjson.SetRawBytes(out, "messages.-1", message)
			}
			deferredMessages = deferredMessages[:0]
		}
		hasAwaitingToolOutput := func() bool {
			for id := range awaitingToolOutputs {
				if _, ok := outputCallIDs[id]; ok {
					return true
				}
			}
			return false
		}
		appendRegularMessage := func(message []byte) {
			// Keep tool-call adjacency strict for providers that require
			// assistant(tool_calls) -> tool(tool_call_id) with no message in between.
			if hasAwaitingToolOutput() {
				deferredMessages = append(deferredMessages, message)
				return
			}
			out, _ = sjson.SetRawBytes(out, "messages.-1", message)
		}
		appendPendingReasoningMessage := func() {
			reasoningContent := takePendingReasoningContent()
			if reasoningContent == "" {
				return
			}
			message := []byte(`{"role":"assistant","content":"","reasoning_content":""}`)
			message, _ = sjson.SetBytes(message, "reasoning_content", reasoningContent)
			appendRegularMessage(message)
		}

		for _, item := range inputItems {
			itemType := item.Get("type").String()
			if itemType == "" && item.Get("role").String() != "" {
				itemType = "message"
			}
			// Keep buffering across BOTH tool-call item types. The case below
			// buffers function_call AND custom_tool_call into pendingToolCalls so
			// consecutive tool calls collapse into one assistant message with
			// multiple tool_calls (correct Chat Completions parallel-call shape).
			// If we only exempt function_call here, a custom_tool_call forces a
			// premature flush and splits consecutive custom tool calls into
			// separate assistant messages before their outputs — invalid ordering.
			if itemType != "function_call" && itemType != "custom_tool_call" {
				flushPendingToolCalls()
			}

			switch itemType {
			case "message", "":
				// Handle regular message conversion
				role := item.Get("role").String()
				if role == "developer" {
					role = "user"
				}
				if role != "assistant" {
					appendPendingReasoningMessage()
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
							if detail := contentItem.Get("detail"); detail.Exists() {
								contentPart, _ = sjson.SetBytes(contentPart, "image_url.detail", detail.String())
							}
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

				if role == "assistant" {
					reasoningContent := item.Get("reasoning_content").String()
					if reasoningContent == "" {
						reasoningContent = takePendingReasoningContent()
					} else {
						pendingReasoningContent = ""
					}
					if reasoningContent != "" {
						message, _ = sjson.SetBytes(message, "reasoning_content", reasoningContent)
					}
				}

				appendRegularMessage(message)

			case "reasoning":
				reasoningContent := collectOpenAIResponsesReasoningContent(item)
				if pendingReasoningContent == "" {
					pendingReasoningContent = reasoningContent
				} else {
					pendingReasoningContent += reasoningContent
				}

			case "function_call", "custom_tool_call":
				// Both function_call (regular OpenAI function tools) AND
				// custom_tool_call (Responses-API custom-grammar tools like
				// ApplyPatch) become an assistant message with tool_calls in
				// chat-completions shape. custom_tool_call uses `input` instead
				// of `arguments` for the tool body — handle both.
				//
				// Upstream buffers consecutive function_call items via
				// pendingToolCalls so they emit as ONE assistant message with
				// multiple tool_calls (correct parallel-function-call shape).
				// Keep that buffering AND the custom_tool_call handling from
				// our migration.
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

				pendingToolCalls = append(pendingToolCalls, gjson.ParseBytes(toolCall).Value())
				if callID := strings.TrimSpace(item.Get("call_id").String()); callID != "" {
					pendingToolCallIDs = append(pendingToolCallIDs, callID)
				}

			case "function_call_output", "custom_tool_call_output":
				// Tool result messages — both standard function tools AND custom
				// tools (e.g. ApplyPatch error messages like "Failed to find
				// context"). Without handling custom_tool_call_output, the
				// model never sees ApplyPatch failure messages and loops trying
				// the same patch repeatedly.
				toolMessage := []byte(`{"role":"tool","tool_call_id":"","content":""}`)
				callID := ""

				if callId := item.Get("call_id"); callId.Exists() {
					callID = strings.TrimSpace(callId.String())
					toolMessage, _ = sjson.SetBytes(toolMessage, "tool_call_id", callID)
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
				if callID != "" {
					delete(awaitingToolOutputs, callID)
				}
				if len(awaitingToolOutputs) == 0 && len(deferredMessages) > 0 {
					flushDeferredMessages()
				}
			}

		}
		flushPendingToolCalls()
		appendPendingReasoningMessage()
		flushDeferredMessages()
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
			// Fork: built-in tools (e.g. {"type":"web_search"},
			// {"type":"apply_patch"}, {"type":"local_shell"}) MUST pass through
			// verbatim. Cursor BYOK (native GPT mode) and other clients rely on
			// these reaching the upstream Codex /responses endpoint, which is
			// the canonical OpenAI API surface that supports them. Upstream's
			// convertResponsesToolToOpenAIChatTools drops unknown types
			// (default: return nil), so we short-circuit the built-ins here
			// BEFORE the helper runs. Only "function"/"namespace"/"" go through
			// upstream's converter; everything else passes verbatim.
			toolType := strings.TrimSpace(tool.Get("type").String())
			switch toolType {
			case "", "function", "namespace":
				for _, chatTool := range convertResponsesToolToOpenAIChatTools(tool) {
					chatCompletionsTools = append(chatCompletionsTools, gjson.ParseBytes(chatTool).Value())
				}
			default:
				if tool.IsObject() {
					chatCompletionsTools = append(chatCompletionsTools, gjson.ParseBytes([]byte(tool.Raw)).Value())
				}
			}
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
		out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(toolChoice.Raw))
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

func collectOpenAIResponsesReasoningContent(item gjson.Result) string {
	var reasoningText strings.Builder
	if summary := item.Get("summary"); summary.Exists() && summary.IsArray() {
		summary.ForEach(func(_, summaryItem gjson.Result) bool {
			if summaryItem.Get("type").String() != "summary_text" {
				return true
			}
			reasoningText.WriteString(summaryItem.Get("text").String())
			return true
		})
	}
	if reasoningText.Len() == 0 {
		return "[reasoning unavailable]"
	}
	return reasoningText.String()
}
