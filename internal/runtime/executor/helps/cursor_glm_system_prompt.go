package helps

// Cursor → GLM system-prompt rewrite. GLM (zai/openai-compatibility) receives the
// SAME Cursor "AI coding assistant, powered by X" prompt family that Cursor sends
// Claude/Opus — verified byte-for-byte from a prod capture (2026-06-28): it has
// `You are an AI coding assistant, powered by GLM 5.2`, `You operate in Cursor.`,
// `<tool_calling>`, `<making_code_changes>`, `<citing_code>`, `<task_management>`,
// and `<mode_selection>`. It does NOT have the GPT-only `<general>` / `<main_goal>`
// markers, so the GPT upgrade path (ApplyCursorGPTSystemPromptUpgrade) bails on it.
//
// This applies the rewrites GLM's prompt structurally supports, matching what the
// other providers get:
//   - shared execution-integrity contract (anchors on </task_management>)
//   - plan-mode redefinition (same generic line Cursor ships)
//   - unexpected-changes patch (same generic line Cursor ships)
//
// Per user instruction (2026-06-28), GLM's identity line is left UNCHANGED — we do
// not rewrite "powered by GLM 5.2" to a different identity.

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// glmMainGoalPlainLine is GLM's plain (untagged) main-goal sentence. Unlike GPT's
// <main_goal>…</main_goal> block, Cursor ships GLM a single line here. We replace
// it with the upgraded behavioral guidance so GLM gets the SAME main-goal
// strengthening GPT gets, without needing the GPT XML wrapper.
const glmMainGoalPlainLine = "Your main goal is to follow the USER's instructions, which are denoted by the <user_query> tag."

// glmMainGoalUpgradedLine mirrors cursorGPTMainGoalPatch's intent but as the
// plain-sentence shape GLM uses (no <main_goal> tags).
const glmMainGoalUpgradedLine = `Your main goal is to satisfy the USER's requested outcome, denoted by the <user_query> tag, end to end.
"End to end" depends on the task mode:
- For review, research, audit, compare, explain, proposal, planning, architecture, migration, or "how should we change this" requests, complete the requested investigation and deliver the analysis or proposal. Do not edit files unless concrete edits are explicitly authorized.
- For implementation, fix, patch, refactor, update, or build requests with a concrete edit target, use available tools to inspect, edit, verify, and finish the work.
- If the user uses broad directional implementation language without a concrete target, the deliverable is a proposal, not a patch.
Attached files, transcripts, logs, screenshots, canvases, reports, or referenced repositories are evidence sources unless the user explicitly names them as the work target. Do not change the work target because an evidence source discusses another project; if the user asks to fix prompt/system instructions using a transcript as evidence, work on the prompt/system-instruction layer and return the analysis/proposed patch.
Following the USER means respecting whether they asked for analysis/proposal or actual code changes. Do not convert a review/proposal request into implementation just because a safe edit is possible.
Only end the turn when the requested outcome for the current task mode is complete, verified where applicable, or blocked by a real missing input, destructive decision, external dependency, or safety constraint.`

// ApplyCursorGLMSystemPromptUpgrade gives a Cursor GLM system prompt the SAME
// behavioral rewrite GPT/Opus get, adapted to GLM's prompt shape. GLM is the
// markdown/<xml_section> "AI coding assistant, powered by X" family (verified
// byte-for-byte from prod 2026-06-28), so every behavioral block GPT gets is
// applied here — APPENDING blocks GLM's prompt does not already contain rather
// than skipping them.
//
// Blocks applied (mirrors the GPT inventory):
//  1. <execution_persistence>          — appended before <mode_selection>
//  2. <execution_integrity_contract>   — appended after </task_management>
//  3. plan-mode line                   — rewritten
//  4. main-goal line                   — upgraded (GLM's plain sentence shape)
//  5. unexpected-changes line          — rewritten if present (GLM may omit it)
//
// Identity is intentionally LEFT UNCHANGED per user instruction (2026-06-28):
// "powered by GLM 5.2" stays. Idempotent; (text, false) when not a Cursor GLM
// prompt.
func ApplyCursorGLMSystemPromptUpgrade(text string) (string, bool) {
	// Gate on the Cursor "AI coding assistant" identity so we never patch a
	// non-Cursor prompt.
	if !strings.Contains(text, cursorAICodingAssistantPrefix) ||
		!strings.Contains(text, cursorOperateLine) {
		return text, false
	}
	// Require the GLM-specific main-goal marker (plain OR already-upgraded, so the
	// helper stays idempotent on re-entry). Without this, the generic Cursor
	// identity/mode scaffolding above would also match non-GLM Cursor prompts and
	// violate this function's "GLM prompt shape" contract.
	if !strings.Contains(text, glmMainGoalPlainLine) &&
		!strings.Contains(text, glmMainGoalUpgradedLine) {
		return text, false
	}
	if !strings.Contains(text, "</task_management>") && !strings.Contains(text, "<mode_selection>") {
		return text, false
	}

	out := text
	changed := false

	// 1. <execution_persistence> — append before <mode_selection> (GLM has it),
	//    matching the GPT placement. Idempotent.
	if !strings.Contains(out, "<execution_persistence>") {
		if modeIdx := strings.Index(out, "<mode_selection>"); modeIdx >= 0 {
			out = out[:modeIdx] + cursorGPTExecutionPersistencePatch + "\n\n" + out[modeIdx:]
			changed = true
		}
	}

	// 2. <execution_integrity_contract> — append after </task_management>
	//    (fallback <mode_selection> / end of prompt). Idempotent.
	if updated, ok := ApplyCursorExecutionIntegrityContract(out); ok {
		out = updated
		changed = true
	}

	// 3. Plan-mode redefinition — same generic line Cursor ships across families.
	oldPlan := "- **Plan**: user asks for a plan, or the task is large/ambiguous or has meaningful trade-offs"
	newPlan := "- **Plan**: use when the user explicitly asks for a plan/design/spec/proposal, when the task is primarily review/research/architecture comparison before implementation, when implementation would require a materially different product/business decision, or when the next step is unsafe/destructive without confirmation. Do not switch to Plan merely because an implementation task is large, multi-step, or requires exploration. For implementation requests, make a brief internal plan and proceed with execution."
	if updated := strings.Replace(out, oldPlan, newPlan, 1); updated != out {
		out = updated
		changed = true
	}

	// 4. Main-goal upgrade — GLM ships a plain sentence (no <main_goal> tags);
	//    replace it with the same behavioral guidance GPT's <main_goal> block has.
	if updated := strings.Replace(out, glmMainGoalPlainLine, glmMainGoalUpgradedLine, 1); updated != out {
		out = updated
		changed = true
	}

	// 5. Unexpected-changes patch — same generic line Cursor ships when present
	//    (GLM's prompt may omit this section entirely; no-op then).
	oldUnexpected := "- While you are working, you might notice unexpected changes that you didn't make. If this happens, STOP IMMEDIATELY and ask the user how they would like to proceed."
	newUnexpected := "- While you are working, you might notice unexpected changes that you didn't make. Do not revert them. If they are unrelated to your task, ignore them and continue. If they overlap files you need to edit, read and understand them, then preserve them while making your changes. Stop and ask only if the unexpected changes directly conflict with the requested work and you cannot safely proceed without choosing between the user's changes and your intended changes."
	if updated := strings.Replace(out, oldUnexpected, newUnexpected, 1); updated != out {
		out = updated
		changed = true
	}

	return out, changed
}

// ApplyCursorGLMSystemPromptUpgradeToPayload rewrites the system message of a
// chat-completions payload (messages[].role == "system"/"developer"). Returns the
// payload unchanged when no Cursor GLM prompt shape is found. Idempotent.
func ApplyCursorGLMSystemPromptUpgradeToPayload(payload []byte) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}

	out := payload
	idx := -1
	messages.ForEach(func(_, msg gjson.Result) bool {
		idx++
		role := strings.ToLower(strings.TrimSpace(msg.Get("role").String()))
		if role != "system" && role != "developer" {
			return true
		}
		content := msg.Get("content")
		// GLM Cursor traffic uses a plain string system content.
		if content.Type == gjson.String {
			if patched, ok := ApplyCursorGLMSystemPromptUpgrade(content.String()); ok {
				if updated, err := sjson.SetBytes(out, fmt.Sprintf("messages.%d.content", idx), patched); err == nil {
					out = updated
				}
			}
			return true
		}
		// Defensive: handle array-of-parts content shape too.
		if content.IsArray() {
			partIdx := -1
			content.ForEach(func(_, part gjson.Result) bool {
				partIdx++
				txt := part.Get("text")
				if txt.Type != gjson.String {
					return true
				}
				if patched, ok := ApplyCursorGLMSystemPromptUpgrade(txt.String()); ok {
					p := fmt.Sprintf("messages.%d.content.%d.text", idx, partIdx)
					if updated, err := sjson.SetBytes(out, p, patched); err == nil {
						out = updated
					}
				}
				return true
			})
		}
		return true
	})
	return out
}
