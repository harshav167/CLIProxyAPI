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
		"Before any tool use, pin three things:",
		"Attached files, transcripts, logs, screenshots, canvases, reports, or repositories are evidence sources unless the user explicitly names them as the work target.",
		"Do not switch the work target to a project, file, repo, or codebase that is merely mentioned inside evidence.",
		"A plan, option menu, checklist, or task list that you generated yourself does not expand the user's original authorization.",
		"If the user challenges the target, scope, task mode, or authorization, stop the disputed work and answer the challenge directly.",
		"Continue only remaining work that is clearly compatible with the user's original request and latest correction",
		"<execution_integrity_contract>",
		"The highest-priority behavioral failure mode to avoid is optimizing for local signs of progress",
		"those structures exist to serve the user's goal; they do not become the goal",
		"<task_target_boundary>",
		"Do not confuse evidence with target.",
		"A transcript about project A can be evidence for fixing prompt layer B",
		"If the user asks for a failure analysis, audit, report, prompt rewrite, or system-instruction proposal, the deliverable is that analysis/proposal",
		"<assistant_generated_choice_rule>",
		"Do not use your own option menu, plan, checklist, or suggested next step to expand scope.",
		"Your todo list is never the contract.",
		"Do not silently substitute weaker verification for stronger required verification.",
		"Before stating a blocker, root cause, or environment explanation as fact, verify it",
		"Do not repeat an unverified explanation as fact in later phases, reports, or handoffs.",
		"A subsystem proof does not satisfy an integrated-system task",
		"do not satisfy it by creating a parallel replacement path unless replacement is explicitly authorized",
		"<user_interruption_rule>",
		"A status, verification, or explanation question temporarily interrupts the work.",
		"A correction updates the active task.",
		"A challenge to the task's legitimacy, target, scope, or authorization suspends the disputed work.",
		"A pause or cancellation suspends or ends the task.",
		"A redirect or materially different requested outcome replaces the active task.",
		"Do not require blanket reauthorization",
		"<subagent_cost_discipline>",
		"Do not launch subagents to rediscover findings already present",
		"Distinguish clearly between locally complete, externally blocked, and globally complete.",
		"Do not switch to Plan merely because an implementation task is large",
		"Do not revert them. If they are unrelated to your task, ignore them and continue.",
		"Your main goal is to satisfy the USER's requested outcome",
		"If the user uses broad directional implementation language without a concrete target, the deliverable is a proposal, not a patch.",
		"Do not convert a review/proposal request into implementation just because a safe edit is possible.",
		"<active_task_contract>",
		"For every request mode, maintain one active task until the user's requested outcome reaches a terminal condition.",
		"This applies equally to implementation, debugging, planning, review, research, audits, explanations, operational work, and artifact refinement.",
		"A phase boundary, progress update, status answer, tool notification, completed subtask, failed attempt with a safe recovery, or discovery that an artifact is stale is not a terminal condition.",
		"A progress update, reviewer verdict, audit synthesis, plan, checklist, draft, or statement of next steps is never a stopping point when the user's requested outcome remains incomplete.",
		"Do not reduce requested scope because the task is large, long-running, multi-step, or crosses research, planning, implementation, verification, or operational phases.",
		"A reviewer verdict, critique, audit finding, subagent synthesis, or rejected draft is intermediate evidence when the requested outcome is an improved, corrected, approved, or workable artifact.",
		"Continue by incorporating the evidence into the requested artifact unless the user asked only for the verdict, critique, audit, or synthesis itself.",
		"When an existing plan, document, specification, checklist, canvas, report, prompt, or other artifact is being refined, amend that same artifact in place.",
		"Do not create a replacement or parallel artifact merely because the existing artifact needs substantial revision.",
		"Review-only constraints permit artifact edits only when refinement of that artifact is part of the user's requested deliverable",
		"If the user requested only findings, a verdict, or no writes, return the analysis without modifying the artifact.",
		"Do not say you will perform an action and then end the turn before performing it.",
		"A status question or system notification temporarily interrupts the active task; it does not replace, cancel, or complete it.",
		"A commentary message is not a handoff to the user.",
		"Before declaring blocked, exhaust safe retries, available evidence, and independent remaining work.",
		"If the user asks to be notified only when done, up, finished, or blocked, suppress all routine progress updates",
		"That authorization persists through retries, rebuilds, verification, status questions, and background notifications.",
		"A correction may preserve, narrow, redirect, or revoke authorization",
		"Attached files, transcripts, logs, screenshots, canvases, reports, or referenced repositories are evidence sources unless the user explicitly names them as the work target.",
		"if the user asks to fix prompt/system instructions using a transcript as evidence, work on the prompt/system-instruction layer",
		"Updates are optional commentary, not completion and not control handoff.",
		"Do not narrate routine reads, builds, polls, retries, tool calls, or elapsed time.",
		"If an update names a next action, perform that action immediately in the same turn.",
		"If the user requests completion-only reporting, remain silent until the requested outcome is verified or genuinely blocked.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("upgraded prompt missing %q:\n%s", want, out)
		}
	}

	for _, unwanted := range []string{
		"<correction_reset_rule>",
		"Explanation is the task until the user explicitly switches back to execution.",
		"do not resume the prior plan, subagents, implementation, or edits;",
		"wait for a new explicit instruction",
		"That authorization persists through retries, corrections",
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
		"<task_target_boundary>",
		"Do not confuse evidence with target.",
		"<user_interruption_rule>",
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

func TestCursorIntegrityContractContainsTranscriptScopeGuards(t *testing.T) {
	input := strings.Join([]string{
		"<task_management>",
		"You have access to the todo_write tool.",
		"</task_management>",
		"",
		"<mcp_file_system>",
		"Tool instructions.",
		"</mcp_file_system>",
	}, "\n")

	out, changed := ApplyCursorExecutionIntegrityContract(input)
	if !changed {
		t.Fatal("expected execution integrity contract insertion")
	}

	for _, want := range []string{
		"<task_target_boundary>",
		"requested deliverable: what the user actually asked to receive;",
		"target: the system, repository, file, prompt layer, API, document, or artifact the user asked you to inspect or modify;",
		"evidence: transcripts, logs, screenshots, canvases, reports, repos, files, or prior conversations supplied to explain the problem;",
		"non-targets: systems or repos mentioned inside evidence but not named as the work target.",
		"Do not confuse evidence with target.",
		"A transcript about project A can be evidence for fixing prompt layer B",
		"If the user asks for a failure analysis, audit, report, prompt rewrite, or system-instruction proposal, the deliverable is that analysis/proposal",
		"<assistant_generated_choice_rule>",
		"Do not use your own option menu, plan, checklist, or suggested next step to expand scope.",
		"<user_interruption_rule>",
		"A status, verification, or explanation question temporarily interrupts the work.",
		"A correction updates the active task.",
		"A challenge to the task's legitimacy, target, scope, or authorization suspends the disputed work.",
		"Do not require blanket reauthorization",
		"<subagent_cost_discipline>",
		"Do not launch subagents to rediscover findings already present",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("integrity contract missing transcript/scope guard %q:\n%s", want, out)
		}
	}
}
