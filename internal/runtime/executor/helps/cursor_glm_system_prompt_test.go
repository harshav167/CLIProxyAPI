package helps

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// glmCursorPromptFixture mirrors the structural shape of the real Cursor → GLM
// system prompt captured from prod (2026-06-28): the "AI coding assistant,
// powered by GLM 5.2" identity family with <task_management>, <mode_selection>,
// and the generic plan-mode line — but NOT the GPT-only <general>/<main_goal>.
const glmCursorPromptFixture = `You are an AI coding assistant, powered by GLM 5.2.

You operate in Cursor.

You are a coding agent in the Cursor IDE that helps the USER with software engineering tasks.

Your main goal is to follow the USER's instructions, which are denoted by the <user_query> tag.

<task_management>
Use the todo_write tool to manage tasks.
</task_management>

<mode_selection>
- **Plan**: user asks for a plan, or the task is large/ambiguous or has meaningful trade-offs
</mode_selection>`

func TestApplyCursorGLMSystemPromptUpgrade_InjectsAllBlocks(t *testing.T) {
	out, ok := ApplyCursorGLMSystemPromptUpgrade(glmCursorPromptFixture)
	if !ok {
		t.Fatalf("expected GLM prompt upgrade to match; got ok=false")
	}
	// 1. execution_persistence appended.
	if !strings.Contains(out, "<execution_persistence>") {
		t.Errorf("expected <execution_persistence> appended")
	}
	// 2. integrity contract appended.
	if !strings.Contains(out, "<execution_integrity_contract>") {
		t.Errorf("expected <execution_integrity_contract> appended")
	}
	// 3. plan-mode rewritten.
	if !strings.Contains(out, "use when the user explicitly asks for a plan/design/spec/proposal") {
		t.Errorf("expected plan-mode redefinition applied")
	}
	// 4. main-goal upgraded (old line gone, new in).
	if strings.Contains(out, "main goal is to follow the USER's instructions, which are denoted") {
		t.Errorf("expected old main-goal line replaced")
	}
	if !strings.Contains(out, "satisfy the USER's requested outcome") {
		t.Errorf("expected upgraded main-goal line")
	}
	// Identity must be left unchanged per user instruction.
	if !strings.Contains(out, "powered by GLM 5.2") {
		t.Errorf("GLM identity must be left unchanged")
	}
	if strings.Contains(out, "You are Claude Code") {
		t.Errorf("GLM identity must NOT be rewritten to Claude Code")
	}
}

func TestApplyCursorGLMSystemPromptUpgrade_Idempotent(t *testing.T) {
	once, ok := ApplyCursorGLMSystemPromptUpgrade(glmCursorPromptFixture)
	if !ok {
		t.Fatal("first pass should match")
	}
	// Second pass must (a) leave the text byte-identical and (b) report no further
	// change (ok=false) — re-applying an already-upgraded prompt is a no-op. The
	// gate must still recognize the upgraded prompt (via glmMainGoalUpgradedLine)
	// rather than bail early, which the byte-equality check below also guards.
	twice, twiceOK := ApplyCursorGLMSystemPromptUpgrade(once)
	if once != twice {
		t.Fatalf("GLM upgrade not idempotent:\nfirst:\n%s\nsecond:\n%s", once, twice)
	}
	if twiceOK {
		t.Fatalf("second pass should report no change (ok=false); got ok=true")
	}
}

func TestApplyCursorGLMSystemPromptUpgrade_NoopForNonCursorPrompt(t *testing.T) {
	in := "You are a helpful assistant."
	out, ok := ApplyCursorGLMSystemPromptUpgrade(in)
	if ok || out != in {
		t.Fatalf("expected no-op for non-Cursor prompt; ok=%v out=%q", ok, out)
	}
}

func TestApplyCursorGLMSystemPromptUpgradeToPayload_PatchesSystemMessage(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":` + jsonQuote(glmCursorPromptFixture) + `},` +
		`{"role":"user","content":"hi"}` +
		`]}`)
	out := ApplyCursorGLMSystemPromptUpgradeToPayload(body)
	sys := gjson.GetBytes(out, "messages.0.content").String()
	if !strings.Contains(sys, "<execution_integrity_contract>") {
		t.Fatalf("expected system message patched in payload; got %s", sys)
	}
	// User message untouched.
	if got := gjson.GetBytes(out, "messages.1.content").String(); got != "hi" {
		t.Fatalf("user message should be unchanged; got %q", got)
	}
}

// jsonQuote produces a JSON string literal for embedding in a raw payload.
func jsonQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}
