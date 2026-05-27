package helps

import (
	"fmt"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// EnsureClaudeCacheControl injects cache_control breakpoints into the payload
// for optimal prompt caching.
func EnsureClaudeCacheControl(payload []byte) []byte {
	payload = InjectClaudeToolsCacheControl(payload)
	payload = InjectClaudeSystemCacheControl(payload)
	payload = InjectClaudeMessagesCacheControl(payload)
	return payload
}

// EnsureClaudeUserPromptCacheAnchor mirrors Claude Code's prompt-anchor pattern:
// the LAST text block of the LAST user message gets a cache_control with
// ttl:"1h".
func EnsureClaudeUserPromptCacheAnchor(payload []byte) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}
	msgs := messages.Array()
	lastUserIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Get("role").String() == "user" {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx < 0 {
		return payload
	}
	content := msgs[lastUserIdx].Get("content")
	if !content.IsArray() {
		return payload
	}
	blocks := content.Array()
	if len(blocks) == 0 {
		return payload
	}
	lastBlockIdx := len(blocks) - 1
	existingTTL := blocks[lastBlockIdx].Get("cache_control.ttl").String()
	if existingTTL == "1h" {
		return payload
	}
	if existingTTL != "" && existingTTL != "1h" {
		return payload
	}
	path := fmt.Sprintf("messages.%d.content.%d.cache_control", lastUserIdx, lastBlockIdx)
	cc := []byte(`{"type":"ephemeral","ttl":"1h"}`)
	updated, err := sjson.SetRawBytes(payload, path, cc)
	if err != nil {
		return payload
	}
	return updated
}

// EnsureCursorClaudeAutomaticPromptCacheControl enables Anthropic's top-level
// automatic prompt caching for Cursor Claude traffic without weakening the
// existing Claude Code-compatible 1h explicit breakpoints.
func EnsureCursorClaudeAutomaticPromptCacheControl(payload []byte) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}
	if gjson.GetBytes(payload, "cache_control").Exists() {
		return payload
	}
	if CountClaudeCacheControls(payload) >= 4 {
		return payload
	}
	if !AllClaudeCacheControlsUseTTL(payload, "1h") {
		return payload
	}
	updated, err := sjson.SetRawBytes(payload, "cache_control", []byte(`{"type":"ephemeral","ttl":"1h"}`))
	if err != nil {
		return payload
	}
	return updated
}

func AllClaudeCacheControlsUseTTL(payload []byte, ttl string) bool {
	found := false
	ok := true
	check := func(item gjson.Result) {
		cc := item.Get("cache_control")
		if !cc.Exists() {
			return
		}
		found = true
		if cc.Get("ttl").String() != ttl {
			ok = false
		}
	}

	if system := gjson.GetBytes(payload, "system"); system.IsArray() {
		system.ForEach(func(_, item gjson.Result) bool {
			check(item)
			return ok
		})
	}
	if tools := gjson.GetBytes(payload, "tools"); ok && tools.IsArray() {
		tools.ForEach(func(_, item gjson.Result) bool {
			check(item)
			return ok
		})
	}
	if messages := gjson.GetBytes(payload, "messages"); ok && messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.IsArray() {
				return ok
			}
			content.ForEach(func(_, item gjson.Result) bool {
				check(item)
				return ok
			})
			return ok
		})
	}

	return found && ok
}

func CountClaudeCacheControls(payload []byte) int {
	count := 0

	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(func(_, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				count++
			}
			return true
		})
	}

	tools := gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		tools.ForEach(func(_, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				count++
			}
			return true
		})
	}

	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			content := msg.Get("content")
			if content.IsArray() {
				content.ForEach(func(_, item gjson.Result) bool {
					if item.Get("cache_control").Exists() {
						count++
					}
					return true
				})
			}
			return true
		})
	}

	return count
}

func NormalizeClaudeCacheControlTTL(payload []byte) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}

	original := payload
	seen5m := false
	modified := false

	processBlock := func(path string, obj gjson.Result) {
		cc := obj.Get("cache_control")
		if !cc.Exists() {
			return
		}
		if !cc.IsObject() {
			seen5m = true
			return
		}
		ttl := cc.Get("ttl")
		if ttl.Type != gjson.String || ttl.String() != "1h" {
			seen5m = true
			return
		}
		if !seen5m {
			return
		}
		ttlPath := path + ".cache_control.ttl"
		updated, errDel := sjson.DeleteBytes(payload, ttlPath)
		if errDel != nil {
			return
		}
		payload = updated
		modified = true
	}

	tools := gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		tools.ForEach(func(idx, item gjson.Result) bool {
			processBlock(fmt.Sprintf("tools.%d", int(idx.Int())), item)
			return true
		})
	}

	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(func(idx, item gjson.Result) bool {
			processBlock(fmt.Sprintf("system.%d", int(idx.Int())), item)
			return true
		})
	}

	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(func(msgIdx, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.IsArray() {
				return true
			}
			content.ForEach(func(itemIdx, item gjson.Result) bool {
				processBlock(fmt.Sprintf("messages.%d.content.%d", int(msgIdx.Int()), int(itemIdx.Int())), item)
				return true
			})
			return true
		})
	}

	if !modified {
		return original
	}
	return payload
}

// EnforceClaudeCacheControlLimit removes excess cache_control blocks so the
// total does not exceed maxBlocks. Breakpoints are collected in Anthropic
// evaluation order (tools → system → messages) and the earliest ones are
// pruned first, preserving the most valuable later breakpoints.
func EnforceClaudeCacheControlLimit(payload []byte, maxBlocks int) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}

	var paths []string
	if tools := gjson.GetBytes(payload, "tools"); tools.IsArray() {
		tools.ForEach(func(idx, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				paths = append(paths, fmt.Sprintf("tools.%d.cache_control", int(idx.Int())))
			}
			return true
		})
	}
	if system := gjson.GetBytes(payload, "system"); system.IsArray() {
		system.ForEach(func(idx, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				paths = append(paths, fmt.Sprintf("system.%d.cache_control", int(idx.Int())))
			}
			return true
		})
	}
	if messages := gjson.GetBytes(payload, "messages"); messages.IsArray() {
		messages.ForEach(func(msgIdx, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.IsArray() {
				return true
			}
			content.ForEach(func(itemIdx, item gjson.Result) bool {
				if item.Get("cache_control").Exists() {
					paths = append(paths, fmt.Sprintf("messages.%d.content.%d.cache_control", int(msgIdx.Int()), int(itemIdx.Int())))
				}
				return true
			})
			return true
		})
	}
	if len(paths) <= maxBlocks {
		return payload
	}
	for _, path := range paths[:len(paths)-maxBlocks] {
		updated, errDel := sjson.DeleteBytes(payload, path)
		if errDel != nil {
			continue
		}
		payload = updated
	}
	return payload
}

func InjectClaudeMessagesCacheControl(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	hasCacheControlInMessages := false
	messages.ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if content.IsArray() {
			content.ForEach(func(_, item gjson.Result) bool {
				if item.Get("cache_control").Exists() {
					hasCacheControlInMessages = true
					return false
				}
				return true
			})
		}
		return !hasCacheControlInMessages
	})
	if hasCacheControlInMessages {
		return payload
	}

	var userMsgIndices []int
	messages.ForEach(func(index gjson.Result, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			userMsgIndices = append(userMsgIndices, int(index.Int()))
		}
		return true
	})

	if len(userMsgIndices) < 2 {
		return payload
	}

	secondToLastUserIdx := userMsgIndices[len(userMsgIndices)-2]
	contentPath := fmt.Sprintf("messages.%d.content", secondToLastUserIdx)
	content := gjson.GetBytes(payload, contentPath)

	if content.IsArray() {
		contentCount := int(content.Get("#").Int())
		if contentCount > 0 {
			cacheControlPath := fmt.Sprintf("messages.%d.content.%d.cache_control", secondToLastUserIdx, contentCount-1)
			result, err := sjson.SetBytes(payload, cacheControlPath, map[string]string{"type": "ephemeral", "scope": "global"})
			if err != nil {
				log.Warnf("failed to inject cache_control into messages: %v", err)
				return payload
			}
			payload = result
		}
	} else if content.Type == gjson.String {
		text := content.String()
		newContent := []map[string]interface{}{
			{
				"type": "text",
				"text": text,
				"cache_control": map[string]string{
					"type":  "ephemeral",
					"scope": "global",
				},
			},
		}
		result, err := sjson.SetBytes(payload, contentPath, newContent)
		if err != nil {
			log.Warnf("failed to inject cache_control into message string content: %v", err)
			return payload
		}
		payload = result
	}

	return payload
}

func InjectClaudeToolsCacheControl(payload []byte) []byte {
	tools := gjson.GetBytes(payload, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return payload
	}

	toolCount := int(tools.Get("#").Int())
	if toolCount == 0 {
		return payload
	}

	hasCacheControlInTools := false
	tools.ForEach(func(_, tool gjson.Result) bool {
		if tool.Get("cache_control").Exists() {
			hasCacheControlInTools = true
			return false
		}
		return true
	})
	if hasCacheControlInTools {
		return payload
	}

	lastToolPath := fmt.Sprintf("tools.%d.cache_control", toolCount-1)
	result, err := sjson.SetBytes(payload, lastToolPath, map[string]string{"type": "ephemeral", "scope": "global"})
	if err != nil {
		log.Warnf("failed to inject cache_control into tools array: %v", err)
		return payload
	}

	return result
}

func InjectClaudeSystemCacheControl(payload []byte) []byte {
	system := gjson.GetBytes(payload, "system")
	if !system.Exists() {
		return payload
	}

	if system.IsArray() {
		count := int(system.Get("#").Int())
		if count == 0 {
			return payload
		}

		hasCacheControlInSystem := false
		system.ForEach(func(_, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				hasCacheControlInSystem = true
				return false
			}
			return true
		})
		if hasCacheControlInSystem {
			return payload
		}

		lastSystemPath := fmt.Sprintf("system.%d.cache_control", count-1)
		result, err := sjson.SetBytes(payload, lastSystemPath, map[string]string{"type": "ephemeral", "scope": "global"})
		if err != nil {
			log.Warnf("failed to inject cache_control into system array: %v", err)
			return payload
		}
		payload = result
	} else if system.Type == gjson.String {
		text := system.String()
		newSystem := []map[string]interface{}{
			{
				"type": "text",
				"text": text,
				"cache_control": map[string]string{
					"type":  "ephemeral",
					"scope": "global",
				},
			},
		}
		result, err := sjson.SetBytes(payload, "system", newSystem)
		if err != nil {
			log.Warnf("failed to inject cache_control into system string: %v", err)
			return payload
		}
		payload = result
	}

	return payload
}
