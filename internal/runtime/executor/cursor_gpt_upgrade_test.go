package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

func TestApplyCursorGPTUpgradeIfEnabledMatchesAnyGPTModel(t *testing.T) {
	payload := []byte(`{"instructions":"<general>\n- Keep working.\n</general>\n\n<mode_selection>\n- **Plan**: user asks for a plan, or the task is large/ambiguous or has meaningful trade-offs\n</mode_selection>\n\n<main_goal>\nYour main goal is to follow the USER's instructions at each message, denoted by the <user_query> tag.\n</main_goal>"}`)
	ctx := WithClientUserAgent(context.Background(), "Cursor/1.0")
	cfg := &config.Config{GPTUpgrade: true}

	for _, model := range []string{"gpt-5.4", "gpt-5.5", "gpt-4o"} {
		out := applyCursorGPTUpgradeIfEnabled(ctx, cfg, model, payload)
		instructions := gjson.GetBytes(out, "instructions").String()
		if !strings.Contains(instructions, "<execution_persistence>") {
			t.Fatalf("expected %s to receive GPT prompt upgrade: %s", model, string(out))
		}
	}
}

func TestApplyCursorGPTUpgradeIfEnabledSkipsNonGPTModels(t *testing.T) {
	payload := []byte(`{"instructions":"<general>\n- Keep working.\n</general>\n\n<mode_selection>\n- **Plan**: user asks for a plan, or the task is large/ambiguous or has meaningful trade-offs\n</mode_selection>\n\n<main_goal>\nYour main goal is to follow the USER's instructions at each message, denoted by the <user_query> tag.\n</main_goal>"}`)
	ctx := WithClientUserAgent(context.Background(), "Cursor/1.0")
	cfg := &config.Config{GPTUpgrade: true}

	for _, model := range []string{"claude-opus-4-7", "gemini-2.5-pro", "codex-5.5-high"} {
		out := applyCursorGPTUpgradeIfEnabled(ctx, cfg, model, payload)
		if string(out) != string(payload) {
			t.Fatalf("expected %s to skip GPT prompt upgrade: %s", model, string(out))
		}
	}
}

func TestApplyCursorGPTUpgradeIfEnabledRequiresCursorAndConfig(t *testing.T) {
	payload := []byte(`{"instructions":"<general>\n- Keep working.\n</general>\n\n<mode_selection>\n- **Plan**: user asks for a plan, or the task is large/ambiguous or has meaningful trade-offs\n</mode_selection>\n\n<main_goal>\nYour main goal is to follow the USER's instructions at each message, denoted by the <user_query> tag.\n</main_goal>"}`)

	cases := []struct {
		name string
		ctx  context.Context
		cfg  *config.Config
	}{
		{name: "non Cursor", ctx: WithClientUserAgent(context.Background(), "factory-cli/0.108.0"), cfg: &config.Config{GPTUpgrade: true}},
		{name: "disabled config", ctx: WithClientUserAgent(context.Background(), "Cursor/1.0"), cfg: &config.Config{}},
		{name: "nil config", ctx: WithClientUserAgent(context.Background(), "Cursor/1.0"), cfg: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := applyCursorGPTUpgradeIfEnabled(tc.ctx, tc.cfg, "gpt-5.5", payload)
			if string(out) != string(payload) {
				t.Fatalf("expected unchanged payload when gate is closed: %s", string(out))
			}
		})
	}
}
