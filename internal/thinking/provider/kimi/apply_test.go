package kimi

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
)

func TestApplyK3UsesRequestedValidEffort(t *testing.T) {
	applier := NewApplier()
	model := &registry.ModelInfo{
		ID: "k3",
		Thinking: &registry.ThinkingSupport{
			Levels: []string{"low", "high", "max"},
		},
	}

	out, err := applier.Apply(
		[]byte(`{"thinking":{"type":"disabled"},"reasoning_effort":"max"}`),
		thinking.ThinkingConfig{Mode: thinking.ModeLevel, Level: thinking.LevelHigh},
		model,
	)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if gjson.GetBytes(out, "reasoning_effort").Exists() {
		t.Fatalf("K3 reasoning_effort should be removed, got %s", out)
	}
	if got := gjson.GetBytes(out, "thinking.type").String(); got != "enabled" {
		t.Fatalf("K3 thinking.type = %q, want enabled; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "thinking.effort").String(); got != "high" {
		t.Fatalf("K3 thinking.effort = %q, want high; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "thinking.keep").String(); got != "all" {
		t.Fatalf("K3 thinking.keep = %q, want all; body=%s", got, out)
	}
}

func TestApplyCodingAliasUsesK2ThinkingObject(t *testing.T) {
	applier := NewApplier()
	model := &registry.ModelInfo{
		ID: "kimi-for-coding",
		Thinking: &registry.ThinkingSupport{
			DynamicAllowed: true,
		},
	}

	out, err := applier.Apply(
		[]byte(`{"reasoning_effort":"high"}`),
		thinking.ThinkingConfig{Mode: thinking.ModeLevel, Level: thinking.LevelHigh},
		model,
	)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if gjson.GetBytes(out, "reasoning_effort").Exists() {
		t.Fatalf("K2.7 Coding reasoning_effort should be removed, got %s", out)
	}
	if got := gjson.GetBytes(out, "thinking.type").String(); got != "enabled" {
		t.Fatalf("K2.7 Coding thinking.type = %q, want enabled; body=%s", got, out)
	}
}

func TestApplyCodingAliasCannotDisableThinking(t *testing.T) {
	applier := NewApplier()
	model := &registry.ModelInfo{
		ID: "kimi-for-coding",
		Thinking: &registry.ThinkingSupport{
			DynamicAllowed: true,
		},
	}

	out, err := applier.Apply(
		[]byte(`{"thinking":{"type":"disabled"}}`),
		thinking.ThinkingConfig{Mode: thinking.ModeNone},
		model,
	)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got := gjson.GetBytes(out, "thinking.type").String(); got != "enabled" {
		t.Fatalf("K2.7 Coding thinking.type = %q, want enabled; body=%s", got, out)
	}
}
