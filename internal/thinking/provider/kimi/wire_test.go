package kimi

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestEnforceModelWireThinkingK3PreservesValidEffort(t *testing.T) {
	out, err := EnforceModelWireThinking(
		[]byte(`{"thinking":{"type":"enabled","effort":"high","keep":"all"},"reasoning_effort":"low"}`),
		"k3",
	)
	if err != nil {
		t.Fatalf("EnforceModelWireThinking() error = %v", err)
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

func TestEnforceModelWireThinkingK3DefaultsInvalidEffortToMax(t *testing.T) {
	out, err := EnforceModelWireThinking(
		[]byte(`{"thinking":{"type":"disabled","effort":"medium","keep":"none"}}`),
		"k3",
	)
	if err != nil {
		t.Fatalf("EnforceModelWireThinking() error = %v", err)
	}
	if got := gjson.GetBytes(out, "thinking.type").String(); got != "enabled" {
		t.Fatalf("K3 thinking.type = %q, want enabled; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "thinking.effort").String(); got != "max" {
		t.Fatalf("K3 thinking.effort = %q, want max; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "thinking.keep").String(); got != "all" {
		t.Fatalf("K3 thinking.keep = %q, want all; body=%s", got, out)
	}
}

func TestEnforceModelWireThinkingCodingAliases(t *testing.T) {
	for _, model := range []string{"kimi-for-coding", "kimi-for-coding-highspeed"} {
		t.Run(model, func(t *testing.T) {
			out, err := EnforceModelWireThinking(
				[]byte(`{"reasoning_effort":"high","thinking":{"type":"disabled"}}`),
				model,
			)
			if err != nil {
				t.Fatalf("EnforceModelWireThinking() error = %v", err)
			}
			if gjson.GetBytes(out, "reasoning_effort").Exists() {
				t.Fatalf("coding alias reasoning_effort should be removed, got %s", out)
			}
			if got := gjson.GetBytes(out, "thinking.type").String(); got != "enabled" {
				t.Fatalf("coding alias thinking.type = %q, want enabled; body=%s", got, out)
			}
		})
	}
}

func TestEnforceModelWireThinkingLeavesUnknownModelUnchanged(t *testing.T) {
	body := []byte(`{"reasoning_effort":"high"}`)
	out, err := EnforceModelWireThinking(body, "custom-kimi-model")
	if err != nil {
		t.Fatalf("EnforceModelWireThinking() error = %v", err)
	}
	if string(out) != string(body) {
		t.Fatalf("unknown model changed: got %s want %s", out, body)
	}
}
