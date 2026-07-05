package helps

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeGLMRequestBody_NoopForOtherProviders(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"service_tier":"priority"}`)
	out := NormalizeGLMRequestBody(body, "openai", "gpt-4o")
	if string(out) != string(body) {
		t.Fatalf("expected unchanged body for non-glm provider; got %s", out)
	}
}

func TestNormalizeGLMRequestBody_StripsUnsupportedFields(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[],"service_tier":"priority","parallel_tool_calls":true,"prompt_cache_key":"abc","store":true,"metadata":{"k":"v"}}`)
	out := NormalizeGLMRequestBody(body, "glm", "glm-5.2")
	for _, field := range []string{"service_tier", "parallel_tool_calls", "prompt_cache_key", "store", "metadata"} {
		if gjson.GetBytes(out, field).Exists() {
			t.Fatalf("expected %s to be stripped; body=%s", field, out)
		}
	}
}

func TestNormalizeGLMRequestBody_CouplesThinkingToReasoningEffort(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[],"reasoning_effort":"high"}`)
	out := NormalizeGLMRequestBody(body, "glm", "glm-5.2")
	thinkingType := gjson.GetBytes(out, "thinking.type").String()
	if thinkingType != "enabled" {
		t.Fatalf("expected thinking.type=enabled when reasoning_effort present; got %s", thinkingType)
	}
}

func TestNormalizeGLMRequestBody_DoesNotEnableThinkingForNoneOrMinimal(t *testing.T) {
	// "none" and "minimal" mean skip thinking entirely. We must NOT inject
	// thinking.type=enabled for these, or a thinking-off request becomes
	// thinking-on upstream.
	for _, level := range []string{"none", "minimal", "NONE", "Minimal"} {
		body := []byte(`{"model":"glm-5.2","messages":[],"reasoning_effort":"` + level + `"}`)
		out := NormalizeGLMRequestBody(body, "glm", "glm-5.2")
		if gjson.GetBytes(out, "thinking").Exists() {
			t.Errorf("reasoning_effort %q: thinking must stay absent; got %s", level, out)
		}
	}
}

func TestNormalizeGLMRequestBody_EnablesThinkingForRealEffortLevels(t *testing.T) {
	for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
		body := []byte(`{"model":"glm-5.2","messages":[],"reasoning_effort":"` + level + `"}`)
		out := NormalizeGLMRequestBody(body, "glm", "glm-5.2")
		if got := gjson.GetBytes(out, "thinking.type").String(); got != "enabled" {
			t.Errorf("reasoning_effort %q: expected thinking.type=enabled; got %q (%s)", level, got, out)
		}
	}
}

func TestNormalizeGLMRequestBody_DoesNotOverwriteCallerThinking(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[],"reasoning_effort":"high","thinking":{"type":"disabled"}}`)
	out := NormalizeGLMRequestBody(body, "glm", "glm-5.2")
	thinkingType := gjson.GetBytes(out, "thinking.type").String()
	if thinkingType != "disabled" {
		t.Fatalf("expected caller-supplied thinking.type to win; got %s", thinkingType)
	}
}

func TestNormalizeGLMRequestBody_DoesNotInjectThinkingWhenEnableThinkingPresent(t *testing.T) {
	// Alibaba Model Studio (token-plan / DashScope compatible-mode) serves the
	// GLM family but uses a flat `enable_thinking` boolean, not z.ai's
	// `thinking.type`. When the caller already set `enable_thinking`, the
	// normalizer must NOT inject the z.ai `thinking` block (Alibaba ignores/
	// rejects it), while still keeping reasoning_effort intact.
	body := []byte(`{"model":"glm-5.2","messages":[],"reasoning_effort":"high","enable_thinking":true}`)
	out := NormalizeGLMRequestBody(body, "openai-compatible-ali", "glm-5.2")
	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("expected NO z.ai thinking block when enable_thinking is set; got %s", gjson.GetBytes(out, "thinking").Raw)
	}
	if got := gjson.GetBytes(out, "enable_thinking").Bool(); got != true {
		t.Fatalf("expected enable_thinking to be preserved; got %v", got)
	}
	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "high" {
		t.Fatalf("expected reasoning_effort=high preserved; got %q", got)
	}
}

func TestNormalizeGLMRequestBody_StillInjectsThinkingForZaiWithoutEnableThinking(t *testing.T) {
	// The z.ai path (no enable_thinking) must be UNCHANGED: reasoning_effort
	// still couples to thinking.type=enabled.
	body := []byte(`{"model":"glm-5.2","messages":[],"reasoning_effort":"high"}`)
	out := NormalizeGLMRequestBody(body, "glm", "glm-5.2")
	if got := gjson.GetBytes(out, "thinking.type").String(); got != "enabled" {
		t.Fatalf("expected z.ai path to still inject thinking.type=enabled; got %q", got)
	}
}

func TestNormalizeGLMRequestBody_MapsReasoningEffortAliases(t *testing.T) {
	cases := map[string]string{
		"low":    "high",
		"medium": "high",
		// "xhigh" must pass through unchanged (NOT remapped to "max"): "max" is
		// rejected by OpenAI-style relay gateways (cline.bot/Vercel) whose enum
		// caps at "xhigh", while "xhigh" is valid on both z.ai-direct and relays.
		"xhigh":   "xhigh",
		"high":    "high",
		"max":     "max",
		"minimal": "minimal",
		"none":    "none",
	}
	for input, expected := range cases {
		body := []byte(`{"model":"glm-5.2","messages":[],"reasoning_effort":"` + input + `"}`)
		out := NormalizeGLMRequestBody(body, "glm", "glm-5.2")
		got := gjson.GetBytes(out, "reasoning_effort").String()
		if got != expected {
			t.Errorf("reasoning_effort %q: expected mapped value %q, got %q", input, expected, got)
		}
	}
}

func TestNormalizeGLMRequestBody_EnablesToolStreamForGLM46Plus(t *testing.T) {
	cases := map[string]bool{
		"glm-5.2": true,
		"glm-5.1": true,
		"glm-5":   true,
		"glm-4.7": true,
		"glm-4.6": true,
		"glm-4.5": false,
		"glm-4":   false,
	}
	tools := `[{"type":"function","function":{"name":"foo","parameters":{}}}]`
	for model, shouldEnable := range cases {
		body := []byte(`{"model":"` + model + `","messages":[],"tools":` + tools + `}`)
		out := NormalizeGLMRequestBody(body, "glm", model)
		got := gjson.GetBytes(out, "tool_stream")
		if shouldEnable {
			if !got.Exists() || !got.Bool() {
				t.Errorf("model %s: expected tool_stream=true; got exists=%v val=%v", model, got.Exists(), got.Bool())
			}
		} else {
			if got.Exists() {
				t.Errorf("model %s: expected tool_stream untouched; got %v", model, got.Bool())
			}
		}
	}
}

func TestNormalizeGLMRequestBody_DoesNotSetToolStreamWithoutTools(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[]}`)
	out := NormalizeGLMRequestBody(body, "glm", "glm-5.2")
	if gjson.GetBytes(out, "tool_stream").Exists() {
		t.Fatalf("expected tool_stream to be absent when no tools present; body=%s", out)
	}
}

func TestNormalizeGLMRequestBody_PreservesCallerToolStream(t *testing.T) {
	tools := `[{"type":"function","function":{"name":"foo","parameters":{}}}]`
	body := []byte(`{"model":"glm-5.2","messages":[],"tools":` + tools + `,"tool_stream":false}`)
	out := NormalizeGLMRequestBody(body, "glm", "glm-5.2")
	if gjson.GetBytes(out, "tool_stream").Bool() {
		t.Fatalf("expected caller-supplied tool_stream=false to win; body=%s", out)
	}
}

func TestNormalizeGLMRequestBody_SortsToolsForCacheStability(t *testing.T) {
	// Intentionally unsorted: zebra, apple, mango. Cache stability requires
	// deterministic ordering — alphabetical by function.name.
	tools := `[
		{"type":"function","function":{"name":"zebra","parameters":{}}},
		{"type":"function","function":{"name":"apple","parameters":{}}},
		{"type":"function","function":{"name":"mango","parameters":{}}}
	]`
	body := []byte(`{"model":"glm-5.2","messages":[],"tools":` + tools + `}`)
	out := NormalizeGLMRequestBody(body, "glm", "glm-5.2")
	names := make([]string, 0, 3)
	for _, t := range gjson.GetBytes(out, "tools").Array() {
		names = append(names, t.Get("function.name").String())
	}
	got := strings.Join(names, ",")
	if got != "apple,mango,zebra" {
		t.Fatalf("expected tools sorted alphabetically by function.name; got %s", got)
	}
}

func TestNormalizeGLMRequestBody_IsIdempotent(t *testing.T) {
	tools := `[
		{"type":"function","function":{"name":"zebra","parameters":{}}},
		{"type":"function","function":{"name":"apple","parameters":{}}}
	]`
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"static"}],"reasoning_effort":"medium","tools":` + tools + `,"service_tier":"priority"}`)
	once := NormalizeGLMRequestBody(body, "glm", "glm-5.2")
	twice := NormalizeGLMRequestBody(once, "glm", "glm-5.2")
	if string(once) != string(twice) {
		t.Fatalf("normalizer not idempotent.\nfirst:  %s\nsecond: %s", once, twice)
	}
}

func TestNormalizeGLMRequestBody_SkipsToolReorderForNonFunctionTools(t *testing.T) {
	// Web-search / retrieval tools don't have a function.name. We must leave
	// the array alone rather than do a partial sort that loses tool semantics.
	tools := `[
		{"type":"web_search","web_search":{}},
		{"type":"function","function":{"name":"foo","parameters":{}}}
	]`
	body := []byte(`{"model":"glm-5.2","messages":[],"tools":` + tools + `}`)
	out := NormalizeGLMRequestBody(body, "glm", "glm-5.2")
	first := gjson.GetBytes(out, "tools.0.type").String()
	if first != "web_search" {
		t.Fatalf("expected first tool to remain web_search (no partial sort); got %s", first)
	}
}

func TestNormalizeGLMRequestBody_ActivatesForGLMFamilyModelOnOtherProvider(t *testing.T) {
	// A throwaway/relay vendor serving GLM under a different provider name must
	// still get the GLM fixes, gated on the model family with zero config.
	cases := []struct {
		provider string
		model    string
		want     bool
	}{
		{"gmi", "zai-org/GLM-5.2-FP8", true},
		{"some-relay", "glm-5.2", true},
		{"openrouter", "z-ai/glm-4.6", true},
		{"openai", "gpt-4o", false},
		{"anthropic", "claude-opus-4-8", false},
	}
	for _, c := range cases {
		// service_tier is stripped only when the normalizer activates.
		body := []byte(`{"model":"` + c.model + `","messages":[],"service_tier":"priority"}`)
		out := NormalizeGLMRequestBody(body, c.provider, c.model)
		stripped := !gjson.GetBytes(out, "service_tier").Exists()
		if stripped != c.want {
			t.Errorf("provider=%s model=%s: activated=%v want=%v (body=%s)", c.provider, c.model, stripped, c.want, out)
		}
	}
}

func TestNormalizeGLMRequestBody_DropsEmptyAssistantContentWithToolCalls(t *testing.T) {
	// The actual prod 400001 repro: assistant with content:[] and tool_calls.
	body := []byte(`{"model":"zai-org/GLM-5.2-FP8","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[],"tool_calls":[{"id":"c1","type":"function","function":{"name":"Shell","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"ok"}` +
		`]}`)
	out := NormalizeGLMRequestBody(body, "gmi", "zai-org/GLM-5.2-FP8")

	if gjson.GetBytes(out, "messages.1.content").Exists() {
		t.Fatalf("expected empty assistant content to be dropped; body=%s", out)
	}
	// tool_calls must be preserved.
	if n := len(gjson.GetBytes(out, "messages.1.tool_calls").Array()); n != 1 {
		t.Fatalf("expected tool_calls preserved (1); got %d: %s", n, out)
	}
}

func TestNormalizeGLMRequestBody_PreservesNonEmptyAssistantContent(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"assistant","content":[{"type":"text","text":"working on it"}],"tool_calls":[{"id":"c1","type":"function","function":{"name":"Shell","arguments":"{}"}}]}` +
		`]}`)
	out := NormalizeGLMRequestBody(body, "glm", "glm-5.2")
	if !gjson.GetBytes(out, "messages.0.content").Exists() {
		t.Fatalf("expected non-empty assistant content to be preserved; body=%s", out)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); got != "working on it" {
		t.Fatalf("content text mangled: %q (%s)", got, out)
	}
}

func TestNormalizeGLMRequestBody_DropsEmptyStringAndNullAssistantContent(t *testing.T) {
	for _, empty := range []string{`""`, `null`, `[]`} {
		body := []byte(`{"model":"glm-5.2","messages":[` +
			`{"role":"assistant","content":` + empty + `,"tool_calls":[{"id":"c1","type":"function","function":{"name":"x","arguments":"{}"}}]}` +
			`]}`)
		out := NormalizeGLMRequestBody(body, "glm", "glm-5.2")
		if gjson.GetBytes(out, "messages.0.content").Exists() {
			t.Errorf("content=%s: expected dropped; got %s", empty, out)
		}
	}
}

func TestNormalizeGLMRequestBody_KeepsEmptyContentWhenNoToolCalls(t *testing.T) {
	// An assistant message with empty content but NO tool_calls is left alone
	// (don't mask a different malformed-request bug).
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"assistant","content":[]}]}`)
	out := NormalizeGLMRequestBody(body, "glm", "glm-5.2")
	if !gjson.GetBytes(out, "messages.0.content").Exists() {
		t.Fatalf("expected empty content kept when no tool_calls; body=%s", out)
	}
}
