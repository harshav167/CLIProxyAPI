// Package openai provides utilities to translate OpenAI Chat Completions
// request JSON into OpenAI Responses API request JSON using gjson/sjson.
// It supports tools, multimodal text/image inputs, and Structured Outputs.
// The package handles the conversion of OpenAI API requests into the format
// expected by the OpenAI Responses API, including proper mapping of messages,
// tools, and generation parameters.
package chat_completions

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertOpenAIRequestToCodex converts an OpenAI Chat Completions request JSON
// into an OpenAI Responses API request JSON. The transformation follows the
// examples defined in docs/2.md exactly, including tools, multi-turn dialog,
// multimodal text/image handling, and Structured Outputs mapping.
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI Chat Completions API
//   - stream: A boolean indicating if the request is for a streaming response
//
// Returns:
//   - []byte: The transformed request data in OpenAI Responses API format
func ConvertOpenAIRequestToCodex(modelName string, inputRawJSON []byte, stream bool) []byte {
	rawJSON := inputRawJSON
	// Start with empty JSON object
	out := []byte(`{"instructions":""}`)

	// Stream must be set to true
	out, _ = sjson.SetBytes(out, "stream", stream)

	// Codex not support temperature, top_p, top_k, max_output_tokens, so comment them
	// if v := gjson.GetBytes(rawJSON, "temperature"); v.Exists() {
	// 	out, _ = sjson.SetBytes(out, "temperature", v.Value())
	// }
	// if v := gjson.GetBytes(rawJSON, "top_p"); v.Exists() {
	// 	out, _ = sjson.SetBytes(out, "top_p", v.Value())
	// }
	// if v := gjson.GetBytes(rawJSON, "top_k"); v.Exists() {
	// 	out, _ = sjson.SetBytes(out, "top_k", v.Value())
	// }

	// Map token limits
	// if v := gjson.GetBytes(rawJSON, "max_tokens"); v.Exists() {
	// 	out, _ = sjson.SetBytes(out, "max_output_tokens", v.Value())
	// }
	// if v := gjson.GetBytes(rawJSON, "max_completion_tokens"); v.Exists() {
	// 	out, _ = sjson.SetBytes(out, "max_output_tokens", v.Value())
	// }

	// Map reasoning effort
	if v := gjson.GetBytes(rawJSON, "reasoning_effort"); v.Exists() {
		out, _ = sjson.SetBytes(out, "reasoning.effort", v.Value())
	} else {
		out, _ = sjson.SetBytes(out, "reasoning.effort", "medium")
	}
	out, _ = sjson.SetBytes(out, "parallel_tool_calls", true)
	out, _ = sjson.SetBytes(out, "reasoning.summary", "auto")
	out, _ = sjson.SetBytes(out, "include", []string{"reasoning.encrypted_content"})

	// Preserve prompt_cache_key through the chat→Codex Responses conversion.
	// Without this, the OpenAI handler's synthetic `cli-proxy-<hash>` injection
	// (or a real client-supplied key from Cursor BYOK) is silently dropped
	// here, and upstream falls back to content-based prefix hashing — which is
	// fragile across turns and gives ~80% cache hits instead of 95%+.
	// The bridge intentionally rejects `cli-proxy-` as a session key (collision
	// safety), but upstream still uses it for content-based cache lookup.
	if v := gjson.GetBytes(rawJSON, "prompt_cache_key"); v.Exists() && v.String() != "" {
		out, _ = sjson.SetBytes(out, "prompt_cache_key", v.String())
	}

	// Preserve service_tier through the chat→Codex Responses conversion.
	// Cursor's "Fast" mode toggle sends `service_tier: "priority"` in the
	// chat-completions request body. Without this passthrough the field is
	// dropped (translator builds a fresh template + only copies whitelisted
	// fields), and upstream defaults to standard tier — so Fast mode silently
	// no-ops on the cli-proxy path. Forwarding only "priority" matches the
	// codex/openai/responses translator's policy which also only preserves
	// "priority" and strips other tier values that Codex /responses rejects.
	if v := gjson.GetBytes(rawJSON, "service_tier"); v.Exists() && v.String() == "priority" {
		out, _ = sjson.SetBytes(out, "service_tier", "priority")
	}

	// Model
	out, _ = sjson.SetBytes(out, "model", modelName)

	// PASSTHROUGH 2026-05-02: removed defensive tool-name-shortening map.
	// Clients that send tools (Cursor, Droid, Codex CLI) already use names
	// within OpenAI's 64-char limit. Building a rewrite map for a no-op
	// rewrite was pointless overhead AND risked breaking ApplyPatch and
	// other native tools that depend on exact name preservation.

	// Extract first system/developer message → top-level `instructions` field
	// (matches Codex CLI's canonical request shape per codex-rs/core/src/client.rs
	// build_responses_request: instructions is a top-level string, not an input
	// message). Without this promotion, the system prompt sits in input[0] as
	// role:"developer" — a different JSON byte structure that hashes to a
	// different prompt-cache shard than Codex CLI's requests, AND gpt-5.5 is
	// trained to give stronger weight to top-level `instructions` than to a
	// developer-role message at position 0. We also skip emitting the promoted
	// message in input[] below to avoid duplication.
	messages := gjson.GetBytes(rawJSON, "messages")
	systemPromotedIdx := -1
	if messages.IsArray() {
		arr := messages.Array()
		for i := 0; i < len(arr); i++ {
			m := arr[i]
			if m.Get("role").String() != "system" {
				continue
			}
			c := m.Get("content")
			var text string
			if c.Type == gjson.String {
				text = c.String()
			} else if c.IsArray() {
				var parts []string
				c.ForEach(func(_, blk gjson.Result) bool {
					if t := blk.Get("text"); t.Exists() && t.Type == gjson.String {
						parts = append(parts, t.String())
					}
					return true
				})
				text = strings.Join(parts, "\n")
			}
			if text != "" {
				out, _ = sjson.SetBytes(out, "instructions", text)
				systemPromotedIdx = i
			}
			break
		}
	}

	// Preserve Responses-native custom tool semantics across the
	// responses->chat->responses transport path. The intermediate chat shape
	// flattens both standard function tools and custom tools into
	// assistant.tool_calls, so we need to recover which calls belong to tools
	// originally registered as non-function Responses tools.
	customToolNames := map[string]bool{}
	customCallIDs := map[string]bool{}
	if rt := gjson.GetBytes(rawJSON, "tools"); rt.IsArray() {
		rt.ForEach(func(_, t gjson.Result) bool {
			tt := t.Get("type").String()
			if tt != "" && tt != "function" {
				if n := t.Get("name").String(); n != "" {
					customToolNames[n] = true
				}
			}
			return true
		})
	}
	if len(customToolNames) > 0 && messages.IsArray() {
		messages.ForEach(func(_, m gjson.Result) bool {
			if m.Get("role").String() != "assistant" {
				return true
			}
			tcs := m.Get("tool_calls")
			if !tcs.IsArray() {
				return true
			}
			tcs.ForEach(func(_, tc gjson.Result) bool {
				if customToolNames[tc.Get("function.name").String()] {
					if id := tc.Get("id").String(); id != "" {
						customCallIDs[id] = true
					}
				}
				return true
			})
			return true
		})
	}

	// Build input from messages, handling all message types including tool calls.
	// Skip the system message that was promoted to top-level `instructions` above
	// to avoid duplicating it as a developer-role entry in input[].
	out, _ = sjson.SetRawBytes(out, "input", []byte(`[]`))
	if messages.IsArray() {
		arr := messages.Array()
		for i := 0; i < len(arr); i++ {
			if i == systemPromotedIdx {
				continue
			}
			m := arr[i]
			role := m.Get("role").String()

			switch role {
			case "tool":
				// Preserve custom tool outputs as custom_tool_call_output so
				// Responses-native tools keep their original transport shape.
				toolCallID := m.Get("tool_call_id").String()
				content := m.Get("content")

				// Branch on custom-tool tracking: tools registered as non-function in
				// rawJSON.tools[] need to round-trip as custom_tool_call_output so
				// Responses-native tools (e.g. ApplyPatch) keep their transport shape.
				if customCallIDs[toolCallID] {
					custOut := []byte(`{"type":"custom_tool_call_output","call_id":"","output":[]}`)
					custOut, _ = sjson.SetBytes(custOut, "call_id", toolCallID)
					blk := []byte(`{"type":"input_text","text":""}`)
					blk, _ = sjson.SetBytes(blk, "text", content.String())
					custOut, _ = sjson.SetRawBytes(custOut, "output.-1", blk)
					out, _ = sjson.SetRawBytes(out, "input.-1", custOut)
				} else {
					// Standard function_call_output via upstream helper which handles
					// string content, array content, image_url, and file parts.
					funcOutput := []byte(`{}`)
					funcOutput, _ = sjson.SetBytes(funcOutput, "type", "function_call_output")
					funcOutput, _ = sjson.SetBytes(funcOutput, "call_id", toolCallID)
					funcOutput = setToolCallOutputContent(funcOutput, content)
					out, _ = sjson.SetRawBytes(out, "input.-1", funcOutput)
				}

			default:
				// Handle regular messages
				msg := []byte(`{}`)
				msg, _ = sjson.SetBytes(msg, "type", "message")
				if role == "system" {
					msg, _ = sjson.SetBytes(msg, "role", "developer")
				} else {
					msg, _ = sjson.SetBytes(msg, "role", role)
				}

				msg, _ = sjson.SetRawBytes(msg, "content", []byte(`[]`))

				// Handle regular content
				c := m.Get("content")
				if c.Exists() && c.Type == gjson.String && c.String() != "" {
					// Single string content
					partType := "input_text"
					if role == "assistant" {
						partType = "output_text"
					}
					part := []byte(`{}`)
					part, _ = sjson.SetBytes(part, "type", partType)
					part, _ = sjson.SetBytes(part, "text", c.String())
					msg, _ = sjson.SetRawBytes(msg, "content.-1", part)
				} else if c.Exists() && c.IsArray() {
					items := c.Array()
					for j := 0; j < len(items); j++ {
						it := items[j]
						t := it.Get("type").String()
						switch t {
						case "text":
							partType := "input_text"
							if role == "assistant" {
								partType = "output_text"
							}
							part := []byte(`{}`)
							part, _ = sjson.SetBytes(part, "type", partType)
							part, _ = sjson.SetBytes(part, "text", it.Get("text").String())
							msg, _ = sjson.SetRawBytes(msg, "content.-1", part)
						case "image_url":
							// Map image inputs to input_image for Responses API
							if role == "user" {
								part := []byte(`{}`)
								part, _ = sjson.SetBytes(part, "type", "input_image")
								if u := it.Get("image_url.url"); u.Exists() {
									part, _ = sjson.SetBytes(part, "image_url", u.String())
								}
								msg, _ = sjson.SetRawBytes(msg, "content.-1", part)
							}
						case "file":
							if role == "user" {
								fileData := it.Get("file.file_data").String()
								filename := it.Get("file.filename").String()
								if fileData != "" {
									part := []byte(`{}`)
									part, _ = sjson.SetBytes(part, "type", "input_file")
									part, _ = sjson.SetBytes(part, "file_data", fileData)
									if filename != "" {
										part, _ = sjson.SetBytes(part, "filename", filename)
									}
									msg, _ = sjson.SetRawBytes(msg, "content.-1", part)
								}
							}
						}
					}
				}

				// Don't emit empty assistant messages when only tool_calls
				// are present — Responses API needs function_call items
				// directly, otherwise call_id matching fails (#2132).
				if role != "assistant" || len(gjson.GetBytes(msg, "content").Array()) > 0 {
					out, _ = sjson.SetRawBytes(out, "input.-1", msg)
				}

				// Handle tool calls for assistant messages as separate top-level objects
				if role == "assistant" {
					toolCalls := m.Get("tool_calls")
					if toolCalls.Exists() && toolCalls.IsArray() {
						toolCallsArr := toolCalls.Array()
						for j := 0; j < len(toolCallsArr); j++ {
							tc := toolCallsArr[j]
							if tc.Get("type").String() == "function" {
								name := tc.Get("function.name").String()
								args := tc.Get("function.arguments").String()
								callID := tc.Get("id").String()
								if customToolNames[name] {
									custCall := []byte(`{"type":"custom_tool_call","call_id":"","name":"","input":""}`)
									custCall, _ = sjson.SetBytes(custCall, "call_id", callID)
									custCall, _ = sjson.SetBytes(custCall, "name", name)
									custCall, _ = sjson.SetBytes(custCall, "input", args)
									out, _ = sjson.SetRawBytes(out, "input.-1", custCall)
									continue
								}

								// Create function_call as top-level object
								funcCall := []byte(`{}`)
								funcCall, _ = sjson.SetBytes(funcCall, "type", "function_call")
								funcCall, _ = sjson.SetBytes(funcCall, "call_id", callID)
								funcCall, _ = sjson.SetBytes(funcCall, "name", name)
								funcCall, _ = sjson.SetBytes(funcCall, "arguments", args)
								out, _ = sjson.SetRawBytes(out, "input.-1", funcCall)
							}
						}
					}
				}
			}
		}
	}

	// Map response_format and text settings to Responses API text.format
	rf := gjson.GetBytes(rawJSON, "response_format")
	text := gjson.GetBytes(rawJSON, "text")
	if rf.Exists() {
		// Always create text object when response_format provided
		if !gjson.GetBytes(out, "text").Exists() {
			out, _ = sjson.SetRawBytes(out, "text", []byte(`{}`))
		}

		rft := rf.Get("type").String()
		switch rft {
		case "text":
			out, _ = sjson.SetBytes(out, "text.format.type", "text")
		case "json_schema":
			js := rf.Get("json_schema")
			if js.Exists() {
				out, _ = sjson.SetBytes(out, "text.format.type", "json_schema")
				if v := js.Get("name"); v.Exists() {
					out, _ = sjson.SetBytes(out, "text.format.name", v.Value())
				}
				if v := js.Get("strict"); v.Exists() {
					out, _ = sjson.SetBytes(out, "text.format.strict", v.Value())
				}
				if v := js.Get("schema"); v.Exists() {
					out, _ = sjson.SetRawBytes(out, "text.format.schema", []byte(v.Raw))
				}
			}
		}

		// Map verbosity if provided
		if text.Exists() {
			if v := text.Get("verbosity"); v.Exists() {
				out, _ = sjson.SetBytes(out, "text.verbosity", v.Value())
			}
		}
	} else if text.Exists() {
		// If only text.verbosity present (no response_format), map verbosity
		if v := text.Get("verbosity"); v.Exists() {
			if !gjson.GetBytes(out, "text").Exists() {
				out, _ = sjson.SetRawBytes(out, "text", []byte(`{}`))
			}
			out, _ = sjson.SetBytes(out, "text.verbosity", v.Value())
		}
	}

	// Map tools (flatten function fields)
	tools := gjson.GetBytes(rawJSON, "tools")
	if tools.IsArray() && len(tools.Array()) > 0 {
		out, _ = sjson.SetRawBytes(out, "tools", []byte(`[]`))
		arr := tools.Array()
		for i := 0; i < len(arr); i++ {
			t := arr[i]
			toolType := t.Get("type").String()
			// Pass through built-in tools (e.g. {"type":"web_search"}) directly for the Responses API.
			// Only "function" needs structural conversion because Chat Completions nests details under "function".
			if toolType != "" && toolType != "function" && t.IsObject() {
				out, _ = sjson.SetRawBytes(out, "tools.-1", []byte(t.Raw))
				continue
			}

			if toolType == "function" {
				item := []byte(`{}`)
				item, _ = sjson.SetBytes(item, "type", "function")
				fn := t.Get("function")
				if fn.Exists() {
					if v := fn.Get("name"); v.Exists() {
						item, _ = sjson.SetBytes(item, "name", v.String())
					}
					if v := fn.Get("description"); v.Exists() {
						item, _ = sjson.SetBytes(item, "description", v.Value())
					}
					if v := fn.Get("parameters"); v.Exists() {
						item, _ = sjson.SetRawBytes(item, "parameters", []byte(v.Raw))
					}
					if v := fn.Get("strict"); v.Exists() {
						item, _ = sjson.SetBytes(item, "strict", v.Value())
					}
				}
				out, _ = sjson.SetRawBytes(out, "tools.-1", item)
			}
		}
	}

	// Map tool_choice when present.
	// Chat Completions: "tool_choice" can be a string ("auto"/"none") or an object (e.g. {"type":"function","function":{"name":"..."}}).
	// Responses API: keep built-in tool choices as-is; flatten function choice to {"type":"function","name":"..."}.
	if tc := gjson.GetBytes(rawJSON, "tool_choice"); tc.Exists() {
		switch {
		case tc.Type == gjson.String:
			out, _ = sjson.SetBytes(out, "tool_choice", tc.String())
		case tc.IsObject():
			tcType := tc.Get("type").String()
			if tcType == "function" {
				name := tc.Get("function.name").String()
				choice := []byte(`{}`)
				choice, _ = sjson.SetBytes(choice, "type", "function")
				if name != "" {
					choice, _ = sjson.SetBytes(choice, "name", name)
				}
				out, _ = sjson.SetRawBytes(out, "tool_choice", choice)
			} else if tcType != "" {
				// Built-in tool choices (e.g. {"type":"web_search"}) are already Responses-compatible.
				out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(tc.Raw))
			}
		}
	}

	out, _ = sjson.SetBytes(out, "store", false)
	return out
}

func setToolCallOutputContent(funcOutput []byte, content gjson.Result) []byte {
	switch {
	case content.Type == gjson.String:
		funcOutput, _ = sjson.SetBytes(funcOutput, "output", content.String())
	case content.IsArray():
		output := []byte(`[]`)
		for _, item := range content.Array() {
			output = appendToolOutputContentPart(output, item)
		}
		funcOutput, _ = sjson.SetRawBytes(funcOutput, "output", output)
	default:
		fallbackOutput := content.Raw
		if fallbackOutput == "" {
			fallbackOutput = content.String()
		}
		funcOutput, _ = sjson.SetBytes(funcOutput, "output", fallbackOutput)
	}
	return funcOutput
}

func appendToolOutputContentPart(output []byte, item gjson.Result) []byte {
	switch item.Get("type").String() {
	case "text":
		part := []byte(`{}`)
		part, _ = sjson.SetBytes(part, "type", "input_text")
		part, _ = sjson.SetBytes(part, "text", item.Get("text").String())
		output, _ = sjson.SetRawBytes(output, "-1", part)
	case "image_url":
		imageURL := item.Get("image_url.url").String()
		fileID := item.Get("image_url.file_id").String()
		if imageURL == "" && fileID == "" {
			return appendToolOutputFallbackPart(output, item)
		}
		part := []byte(`{}`)
		part, _ = sjson.SetBytes(part, "type", "input_image")
		if imageURL != "" {
			part, _ = sjson.SetBytes(part, "image_url", imageURL)
		}
		if fileID != "" {
			part, _ = sjson.SetBytes(part, "file_id", fileID)
		}
		if detail := item.Get("image_url.detail").String(); detail != "" {
			part, _ = sjson.SetBytes(part, "detail", detail)
		}
		output, _ = sjson.SetRawBytes(output, "-1", part)
	case "file":
		fileID := item.Get("file.file_id").String()
		fileData := item.Get("file.file_data").String()
		fileURL := item.Get("file.file_url").String()
		if fileID == "" && fileData == "" && fileURL == "" {
			return appendToolOutputFallbackPart(output, item)
		}
		part := []byte(`{}`)
		part, _ = sjson.SetBytes(part, "type", "input_file")
		if fileID != "" {
			part, _ = sjson.SetBytes(part, "file_id", fileID)
		}
		if fileData != "" {
			part, _ = sjson.SetBytes(part, "file_data", fileData)
		}
		if fileURL != "" {
			part, _ = sjson.SetBytes(part, "file_url", fileURL)
		}
		if filename := item.Get("file.filename").String(); filename != "" {
			part, _ = sjson.SetBytes(part, "filename", filename)
		}
		output, _ = sjson.SetRawBytes(output, "-1", part)
	default:
		output = appendToolOutputFallbackPart(output, item)
	}
	return output
}

func appendToolOutputFallbackPart(output []byte, item gjson.Result) []byte {
	text := item.Raw
	if text == "" {
		text = item.String()
	}
	part := []byte(`{}`)
	part, _ = sjson.SetBytes(part, "type", "input_text")
	part, _ = sjson.SetBytes(part, "text", text)
	output, _ = sjson.SetRawBytes(output, "-1", part)
	return output
}
