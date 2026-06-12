package helps

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestIsCursorFableAliasModel(t *testing.T) {
	cases := map[string]bool{
		"f5-low":                  true,
		"f5-medium":               true,
		"f5-high":                 true,
		"f5-xhigh":                true,
		"f5-max":                  true,
		"  f5-max  ":              true,
		"claude-fable-5":          false,
		"claude-fable-5-thinking": false,
		"claude-opus-4-7":         false,
		"gpt-5.4":                 false,
		"":                        false,
		"f5":                      false,
	}
	for model, want := range cases {
		if got := IsCursorFableAliasModel(model); got != want {
			t.Errorf("IsCursorFableAliasModel(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestApplyCursorFableAliasSnapshotReplacesSystemAndTools(t *testing.T) {
	payload := []byte(`{` +
		`"model":"f5-max",` +
		`"system":[{"type":"text","text":"original cursor BYOK system"}],` +
		`"tools":[{"name":"OnlyOne"}],` +
		`"messages":[{"role":"user","content":"hi"}],` +
		`"max_tokens":1234` +
		`}`)

	out := ApplyCursorFableAliasSnapshot(payload, "f5-max")

	system := gjson.GetBytes(out, "system")
	if !system.IsArray() {
		t.Fatalf("system not an array after swap: %s", system.Raw)
	}
	if got, want := system.Get("0.text").String()[:40], "You are an AI coding assistant, powered "; got != want {
		t.Fatalf("system[0].text prefix = %q, want %q", got, want)
	}
	if got, want := system.Get("0.text").String(), "Claude Fable 5"; !contains(got, want) {
		t.Fatalf("system[0].text does not mention %q (got %d-byte block)", want, len(got))
	}

	tools := gjson.GetBytes(out, "tools")
	if !tools.IsArray() {
		t.Fatalf("tools not an array after swap: %s", tools.Raw)
	}
	if got := len(tools.Array()); got <= 1 {
		t.Fatalf("tools.len = %d, want > 1 (snapshot ships many native tools)", got)
	}
	names := map[string]bool{}
	tools.ForEach(func(_, v gjson.Result) bool {
		names[v.Get("name").String()] = true
		return true
	})
	for _, want := range []string{"Shell", "Read", "Grep", "TodoWrite"} {
		if !names[want] {
			t.Errorf("tools array missing %q (got %d names)", want, len(names))
		}
	}

	if got, want := gjson.GetBytes(out, "messages.0.content").String(), "hi"; got != want {
		t.Fatalf("messages.0.content = %q, want %q (unrelated field clobbered)", got, want)
	}
	if got, want := gjson.GetBytes(out, "max_tokens").Int(), int64(1234); got != want {
		t.Fatalf("max_tokens = %d, want %d (unrelated field clobbered)", got, want)
	}
	if got, want := gjson.GetBytes(out, "model").String(), "f5-max"; got != want {
		t.Fatalf("model = %q, want %q (model field is rewritten downstream, not here)", got, want)
	}
}

func TestApplyCursorFableAliasSnapshotLeavesNonAliasUntouched(t *testing.T) {
	original := []byte(`{"model":"claude-opus-4-7-thinking-max","system":[{"type":"text","text":"keep me"}],"tools":[{"name":"KeepMe"}]}`)
	got := ApplyCursorFableAliasSnapshot(original, "claude-opus-4-7-thinking-max")
	if string(got) != string(original) {
		t.Fatalf("non-alias payload was modified.\n got: %s\nwant: %s", got, original)
	}
}

func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
