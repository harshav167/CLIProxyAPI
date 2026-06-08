package helps

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestRewriteCursorSystemPromptIdentity_RewritesOnlyIdentityLines(t *testing.T) {
	input := strings.Join([]string{
		"You are an AI coding assistant, powered by Opus 4.6.",
		"",
		"You operate in Cursor.",
		"",
		"You are a coding agent in the Cursor IDE that helps the USER with software engineering tasks.",
		"",
		"Use cursor-app-control and cursor-ide-browser.",
		"Project metadata lives under .cursor/projects/example.",
		"Cursor UI references should remain because they describe the host surface.",
	}, "\n")

	out, matched := RewriteCursorSystemPromptIdentity(input)
	if !matched {
		t.Fatal("expected Cursor raw prompt shape to match")
	}

	for _, want := range []string{
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You operate in Claude Code.",
		"You are a coding agent in Claude Code that helps the USER with software engineering tasks.",
		"cursor-app-control",
		"cursor-ide-browser",
		".cursor/projects/example",
		"Cursor UI references should remain",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rewritten prompt missing %q:\n%s", want, out)
		}
	}

	for _, unwanted := range []string{
		"You are an AI coding assistant, powered by Opus 4.6.",
		"You operate in Cursor.",
		"You are a coding agent in the Cursor IDE that helps the USER with software engineering tasks.",
	} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("rewritten prompt still contains %q:\n%s", unwanted, out)
		}
	}
}

func TestRewriteCursorSystemPromptIdentity_NonCursorUnchanged(t *testing.T) {
	input := "You are a helpful assistant.\nYou operate in another client."

	out, matched := RewriteCursorSystemPromptIdentity(input)
	if matched {
		t.Fatal("non-Cursor prompt should not match")
	}
	if out != input {
		t.Fatalf("non-Cursor prompt changed:\nwant %q\ngot  %q", input, out)
	}
}

func TestApplyCursorGPTSystemPromptUpgrade(t *testing.T) {
	input := strings.Join([]string{
		"You are running as a coding agent in Cursor IDE on a user's computer.",
		"",
		"<general>",
		"- Keep working.",
		"</general>",
		"",
		"<editing_constraints>",
		"- While you are working, you might notice unexpected changes that you didn't make. If this happens, STOP IMMEDIATELY and ask the user how they would like to proceed.",
		"</editing_constraints>",
		"",
		"<mode_selection>",
		"- **Plan**: user asks for a plan, or the task is large/ambiguous or has meaningful trade-offs",
		"</mode_selection>",
		"",
		"<working_with_the_user>",
		"## Final answer",
		"- Be concise.",
		"",
		"## Intermediary updates",
		"- Intermediary updates go to the `commentary` channel.",
		"- As you are thinking, you very frequently provide updates even if not taking any actions, informing the user of your progress. You interrupt your thinking and send multiple updates in a row if thinking for more than 100 words.",
		"</working_with_the_user>",
		"",
		"<main_goal>",
		"Your main goal is to follow the USER's instructions at each message, denoted by the <user_query> tag.",
		"</main_goal>",
	}, "\n")

	out, matched := ApplyCursorGPTSystemPromptUpgrade(input)
	if !matched {
		t.Fatal("expected Cursor GPT prompt shape to match")
	}

	for _, want := range []string{
		"<execution_persistence>",
		"First classify the user's requested outcome before acting:",
		"Before classifying a request as implementation, determine whether the user authorized concrete edits.",
		"Broad directional language is not concrete edit authorization by itself",
		"For broad architecture or migration requests, inspect the code and produce a concrete proposal first.",
		"Do not edit files, apply patches, refactor code, or run write/mutation tools unless the user explicitly asks for implementation.",
		"Do not treat \"how would you implement\", \"proposal\", \"recommend\", \"bridge the gap\", or \"optimize\" as permission to edit.",
		"Use this mode only when the user has authorized concrete edits or the requested outcome necessarily requires code changes now.",
		"If the request contains both review/comparison language and broad implementation language, do not edit unless the implementation target is concrete and unambiguous.",
		"<execution_integrity_contract>",
		"The highest-priority behavioral failure mode to avoid is optimizing for local signs of progress",
		"those structures exist to serve the user's goal; they do not become the goal",
		"Your todo list is never the contract.",
		"Do not silently substitute weaker verification for stronger required verification.",
		"Before stating a blocker, root cause, or environment explanation as fact, verify it",
		"Do not repeat an unverified explanation as fact in later phases, reports, or handoffs.",
		"A subsystem proof does not satisfy an integrated-system task",
		"do not satisfy it by creating a parallel replacement path unless replacement is explicitly authorized",
		"If the user asks a question, answer the question first.",
		"Distinguish clearly between locally complete, externally blocked, and globally complete.",
		"Do not switch to Plan merely because an implementation task is large",
		"Do not revert them. If they are unrelated to your task, ignore them and continue.",
		"Your main goal is to satisfy the USER's requested outcome",
		"If the user uses broad directional implementation language without a concrete target, the deliverable is a proposal, not a patch.",
		"Do not convert a review/proposal request into implementation just because a safe edit is possible.",
		"Do not announce that you will edit, patch, refactor, or implement unless the user requested concrete edits.",
		"Do not send repeated updates while merely thinking.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("upgraded prompt missing %q:\n%s", want, out)
		}
	}

	for _, unwanted := range []string{
		"STOP IMMEDIATELY",
		"the task is large/ambiguous",
		"As you are thinking, you very frequently provide updates",
		"Your main goal is to follow the USER's instructions",
	} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("upgraded prompt still contains %q:\n%s", unwanted, out)
		}
	}

	again, matchedAgain := ApplyCursorGPTSystemPromptUpgrade(out)
	if matchedAgain {
		t.Fatal("second upgrade should be idempotent")
	}
	if again != out {
		t.Fatal("second upgrade changed already-upgraded prompt")
	}
}

func TestApplyCursorGPTSystemPromptUpgradeToPayload(t *testing.T) {
	prompt := strings.Join([]string{
		"<general>",
		"- Keep working.",
		"</general>",
		"",
		"<mode_selection>",
		"- **Plan**: user asks for a plan, or the task is large/ambiguous or has meaningful trade-offs",
		"</mode_selection>",
		"",
		"<main_goal>",
		"Your main goal is to follow the USER's instructions at each message, denoted by the <user_query> tag.",
		"</main_goal>",
	}, "\n")
	payload := []byte(`{"instructions":""}`)
	payload, _ = sjson.SetBytes(payload, "instructions", prompt)

	out := ApplyCursorGPTSystemPromptUpgradeToPayload(payload)
	instructions := gjson.GetBytes(out, "instructions").String()
	if !strings.Contains(instructions, "<execution_persistence>") {
		t.Fatalf("expected payload instructions to be upgraded: %s", string(out))
	}
	if !strings.Contains(instructions, "<execution_integrity_contract>") {
		t.Fatalf("expected payload instructions to include execution integrity contract: %s", string(out))
	}

	other := []byte(`{"instructions":"You are helpful."}`)
	if got := string(ApplyCursorGPTSystemPromptUpgradeToPayload(other)); got != string(other) {
		t.Fatalf("non-Cursor payload changed: %s", got)
	}
}

func TestRewriteCursorSystemPromptIdentityAndIntegrity(t *testing.T) {
	input := strings.Join([]string{
		"You are an AI coding assistant, powered by Opus 4.7.",
		"",
		"You operate in Cursor.",
		"",
		"You are a coding agent in the Cursor IDE that helps the USER with software engineering tasks.",
		"",
		"<task_management>",
		"You have access to the todo_write tool.",
		"</task_management>",
		"",
		"<mcp_file_system>",
		"Tool instructions.",
		"</mcp_file_system>",
	}, "\n")

	out, matched := RewriteCursorSystemPromptIdentityAndIntegrity(input)
	if !matched {
		t.Fatal("expected Cursor raw prompt shape to match")
	}

	for _, want := range []string{
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You operate in Claude Code.",
		"<execution_integrity_contract>",
		"Your todo list is never the contract.",
		"</execution_integrity_contract>\n\n<mcp_file_system>",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rewritten prompt missing %q:\n%s", want, out)
		}
	}

	again, changedAgain := ApplyCursorExecutionIntegrityContract(out)
	if changedAgain {
		t.Fatal("execution integrity contract should be idempotent")
	}
	if strings.Count(again, "<execution_integrity_contract>") != 1 {
		t.Fatalf("integrity contract should not duplicate:\n%s", again)
	}
}
