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

func TestNormalizeGLMRequestBody_MapsReasoningEffortAliases(t *testing.T) {
	cases := map[string]string{
		"low":     "high",
		"medium":  "high",
		"xhigh":   "max",
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
