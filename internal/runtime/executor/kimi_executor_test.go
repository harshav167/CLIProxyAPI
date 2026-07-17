package executor

import (
	"net/http"
	"strings"
	"testing"

	kimithinking "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/kimi"
	"github.com/tidwall/gjson"
)

func TestKimiPostOverrideThinking_ConfiguresK3WithCallerEffort(t *testing.T) {
	out, err := kimithinking.EnforceModelWireThinking(
		[]byte(`{"messages":[],"thinking":{"effort":"low"}}`),
		"k3",
	)
	if err != nil {
		t.Fatalf("EnforceModelWireThinking() error = %v", err)
	}
	if gjson.GetBytes(out, "reasoning_effort").Exists() {
		t.Fatalf("reasoning_effort should be absent; body=%s", out)
	}
	if got := gjson.GetBytes(out, "thinking.type").String(); got != "enabled" {
		t.Fatalf("thinking.type = %q, want enabled; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "thinking.effort").String(); got != "low" {
		t.Fatalf("thinking.effort = %q, want low; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "thinking.keep").String(); got != "all" {
		t.Fatalf("thinking.keep = %q, want all; body=%s", got, out)
	}
}

func TestKimiPostOverrideThinking_ForcesCodingAliasThinking(t *testing.T) {
	out, err := kimithinking.EnforceModelWireThinking(
		[]byte(`{"reasoning_effort":"high","thinking":{"type":"disabled"}}`),
		"kimi-for-coding",
	)
	if err != nil {
		t.Fatalf("EnforceModelWireThinking() error = %v", err)
	}
	if gjson.GetBytes(out, "reasoning_effort").Exists() {
		t.Fatalf("K2.7 Coding reasoning_effort should be absent; body=%s", out)
	}
	if got := gjson.GetBytes(out, "thinking.type").String(); got != "enabled" {
		t.Fatalf("thinking.type = %q, want enabled; body=%s", got, out)
	}
}

func TestApplyKimiHeadersMatchesCurrentKimiCodeCLI(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://api.kimi.com/coding/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}

	applyKimiHeaders(req, "token", true)

	want := map[string]string{
		"User-Agent":                  "kimi-code-cli/0.26.0",
		"X-Msh-Platform":              "kimi_code_cli",
		"X-Msh-Version":               "0.26.0",
		"X-Msh-Device-Model":          "macOS 27.0 arm64",
		"X-Msh-Os-Version":            "27.0.0",
		"X-Stainless-Retry-Count":     "0",
		"X-Stainless-Lang":            "js",
		"X-Stainless-Package-Version": "6.34.0",
		"X-Stainless-Os":              "MacOS",
		"X-Stainless-Arch":            "arm64",
		"X-Stainless-Runtime":         "node",
		"X-Stainless-Runtime-Version": "v24.18.0",
		"Accept":                      "text/event-stream",
	}
	for name, expected := range want {
		if got := req.Header.Get(name); got != expected {
			t.Fatalf("%s = %q, want %q", name, got, expected)
		}
	}
}

func TestApplyKimiHeadersUsesJSONAcceptForNonStream(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://api.kimi.com/coding/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}

	applyKimiHeaders(req, "token", false)

	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q, want application/json", got)
	}
}

// TestNormalizeKimiToolSchemaRefs_InlinesSiblingRef reproduces the exact prod
// failure: Cursor's UpdateCurrentStep tool has final_summary / completed_subtitle
// referencing a sibling property via {"$ref":"#/properties/current_step"}, with
// no $defs block. Moonshot rejects any $ref not under #/$defs/. After
// normalisation there must be no $ref, and the ref-ing properties must inherit
// current_step's type while keeping their own description.
func TestNormalizeKimiToolSchemaRefs_InlinesSiblingRef(t *testing.T) {
	body := []byte(`{"model":"kimi-k2.7-code","messages":[],"tools":[{"type":"function","function":{"name":"UpdateCurrentStep","parameters":{"type":"object","properties":{"current_step":{"type":"string","minLength":1,"description":"Major step"},"final_summary":{"$ref":"#/properties/current_step","description":"Exec summary"},"completed_subtitle":{"$ref":"#/properties/current_step","description":"Past-tense subtitle"}}}}}]}`)

	out := normalizeKimiToolSchemaRefs(body)

	if strings.Contains(string(out), `"$ref"`) {
		t.Fatalf("expected all $ref removed, got: %s", out)
	}
	p := "tools.0.function.parameters.properties."
	if got := gjson.GetBytes(out, p+"final_summary.type").String(); got != "string" {
		t.Fatalf("final_summary.type = %q, want string", got)
	}
	if got := gjson.GetBytes(out, p+"final_summary.minLength").Int(); got != 1 {
		t.Fatalf("final_summary.minLength = %d, want 1 (inherited)", got)
	}
	if got := gjson.GetBytes(out, p+"final_summary.description").String(); got != "Exec summary" {
		t.Fatalf("final_summary.description = %q, want local override 'Exec summary'", got)
	}
	if got := gjson.GetBytes(out, p+"completed_subtitle.type").String(); got != "string" {
		t.Fatalf("completed_subtitle.type = %q, want string", got)
	}
	if got := gjson.GetBytes(out, p+"completed_subtitle.description").String(); got != "Past-tense subtitle" {
		t.Fatalf("completed_subtitle.description = %q, want local override", got)
	}
	// current_step (the ref target) must be untouched.
	if got := gjson.GetBytes(out, p+"current_step.description").String(); got != "Major step" {
		t.Fatalf("current_step.description = %q, want 'Major step'", got)
	}
}

// $defs refs are already Moonshot-valid and must be left as-is.
func TestNormalizeKimiToolSchemaRefs_LeavesDefsRefsAlone(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","function":{"name":"T","parameters":{"type":"object","properties":{"x":{"$ref":"#/$defs/Foo"}},"$defs":{"Foo":{"type":"string"}}}}}]}`)
	out := normalizeKimiToolSchemaRefs(body)
	if got := gjson.GetBytes(out, "tools.0.function.parameters.properties.x.$ref").String(); got != "#/$defs/Foo" {
		t.Fatalf("valid #/$defs/ ref should be preserved, got %q (body=%s)", got, out)
	}
}

// No tools / no refs => untouched.
func TestNormalizeKimiToolSchemaRefs_Noop(t *testing.T) {
	body := []byte(`{"model":"kimi-k2.7-code","messages":[{"role":"user","content":"hi"}]}`)
	if got := normalizeKimiToolSchemaRefs(body); string(got) != string(body) {
		t.Fatalf("expected no-op, body changed:\n in=%s\nout=%s", body, got)
	}
}

func TestEnsureKimiPromptCacheKey_InjectsStableKey(t *testing.T) {
	// Two turns of the SAME conversation: same system + first user, different
	// trailing messages. They must produce the SAME prompt_cache_key so Kimi
	// sticky-routes them to the same prefix-cache shard.
	turn1 := []byte(`{"messages":[{"role":"system","content":"sys prompt"},{"role":"user","content":"do X"}]}`)
	turn2 := []byte(`{"messages":[{"role":"system","content":"sys prompt"},{"role":"user","content":"do X"},{"role":"assistant","content":"ok"},{"role":"user","content":"now Y"}]}`)

	k1 := gjson.GetBytes(ensureKimiPromptCacheKey(turn1), "prompt_cache_key").String()
	k2 := gjson.GetBytes(ensureKimiPromptCacheKey(turn2), "prompt_cache_key").String()
	if k1 == "" {
		t.Fatal("expected prompt_cache_key to be injected")
	}
	if k1 != k2 {
		t.Fatalf("prompt_cache_key not stable across turns: %q vs %q", k1, k2)
	}

	// A different conversation (different first user message) must get a different key.
	other := []byte(`{"messages":[{"role":"system","content":"sys prompt"},{"role":"user","content":"different task"}]}`)
	k3 := gjson.GetBytes(ensureKimiPromptCacheKey(other), "prompt_cache_key").String()
	if k3 == k1 {
		t.Fatalf("expected different conversation to get a different key, both were %q", k1)
	}
}

func TestEnsureKimiPromptCacheKey_DoesNotOverwriteCaller(t *testing.T) {
	body := []byte(`{"prompt_cache_key":"caller-set","messages":[{"role":"user","content":"hi"}]}`)
	got := gjson.GetBytes(ensureKimiPromptCacheKey(body), "prompt_cache_key").String()
	if got != "caller-set" {
		t.Fatalf("caller prompt_cache_key overwritten: got %q, want %q", got, "caller-set")
	}
}

func TestNormalizeKimiToolMessageLinks_UsesCallIDFallback(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"list_directory:1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]},
			{"role":"tool","call_id":"list_directory:1","content":"[]"}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	got := gjson.GetBytes(out, "messages.1.tool_call_id").String()
	if got != "list_directory:1" {
		t.Fatalf("messages.1.tool_call_id = %q, want %q", got, "list_directory:1")
	}
}

func TestNormalizeKimiToolMessageLinks_InferSinglePendingID(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_123","type":"function","function":{"name":"read_file","arguments":"{}"}}]},
			{"role":"tool","content":"file-content"}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	got := gjson.GetBytes(out, "messages.1.tool_call_id").String()
	if got != "call_123" {
		t.Fatalf("messages.1.tool_call_id = %q, want %q", got, "call_123")
	}
}

func TestNormalizeKimiToolMessageLinks_AmbiguousMissingIDIsNotInferred(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}},
				{"id":"call_2","type":"function","function":{"name":"read_file","arguments":"{}"}}
			]},
			{"role":"tool","content":"result-without-id"}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	if gjson.GetBytes(out, "messages.1.tool_call_id").Exists() {
		t.Fatalf("messages.1.tool_call_id should be absent for ambiguous case, got %q", gjson.GetBytes(out, "messages.1.tool_call_id").String())
	}
}

func TestNormalizeKimiToolMessageLinks_PreservesExistingToolCallID(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","call_id":"different-id","content":"result"}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	got := gjson.GetBytes(out, "messages.1.tool_call_id").String()
	if got != "call_1" {
		t.Fatalf("messages.1.tool_call_id = %q, want %q", got, "call_1")
	}
}

func TestNormalizeKimiToolMessageLinks_DoesNotInheritOtherTurnReasoning(t *testing.T) {
	// A tool-call turn with no reasoning of its own must NOT inherit an earlier
	// turn's reasoning. Copying "previous reasoning" onto message[1] pairs it with
	// the wrong tool action and produced the confused, rambling reasoning users
	// saw. message[1] has no own content, so it gets the neutral placeholder.
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":"plan","reasoning_content":"previous reasoning"},
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	// message[1] must NOT inherit message[0]'s "previous reasoning"; it gets
	// reasoning synthesised from its OWN tool call instead.
	wantSynth := "I'll use the list_directory tool to make progress on the task."
	if got := gjson.GetBytes(out, "messages.1.reasoning_content").String(); got != wantSynth {
		t.Fatalf("messages.1.reasoning_content = %q, want %q (synthesised from own tool call, not inherited)", got, wantSynth)
	}
	if strings.Contains(gjson.GetBytes(out, "messages.1.reasoning_content").String(), "continuing") {
		t.Fatalf("reasoning must never be a bare (continuing) marker")
	}
	if got := gjson.GetBytes(out, "messages.0.reasoning_content").String(); got != "previous reasoning" {
		t.Fatalf("messages.0.reasoning_content = %q, want %q (original must be intact)", got, "previous reasoning")
	}
}

func TestNormalizeKimiToolMessageLinks_InsertsFallbackReasoningWhenMissing(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	reasoning := gjson.GetBytes(out, "messages.0.reasoning_content")
	if !reasoning.Exists() {
		t.Fatalf("messages.0.reasoning_content should exist")
	}
	want := "I'll use the list_directory tool to make progress on the task."
	if reasoning.String() != want {
		t.Fatalf("messages.0.reasoning_content = %q, want %q", reasoning.String(), want)
	}
}

func TestNormalizeKimiToolMessageLinks_UsesContentAsReasoningFallback(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":[{"type":"text","text":"first line"},{"type":"text","text":"second line"}],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	got := gjson.GetBytes(out, "messages.0.reasoning_content").String()
	if got != "first line\nsecond line" {
		t.Fatalf("messages.0.reasoning_content = %q, want %q", got, "first line\nsecond line")
	}
}

func TestNormalizeKimiToolMessageLinks_ReplacesEmptyReasoningContent(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":"assistant summary","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}],"reasoning_content":""}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	got := gjson.GetBytes(out, "messages.0.reasoning_content").String()
	if got != "assistant summary" {
		t.Fatalf("messages.0.reasoning_content = %q, want %q", got, "assistant summary")
	}
}

func TestNormalizeKimiToolMessageLinks_PreservesExistingAssistantReasoning(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}],"reasoning_content":"keep me"}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	got := gjson.GetBytes(out, "messages.0.reasoning_content").String()
	if got != "keep me" {
		t.Fatalf("messages.0.reasoning_content = %q, want %q", got, "keep me")
	}
}

func TestNormalizeKimiToolMessageLinks_RepairsIDsAndReasoningTogether(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}],"reasoning_content":"r1"},
			{"role":"tool","call_id":"call_1","content":"[]"},
			{"role":"assistant","tool_calls":[{"id":"call_2","type":"function","function":{"name":"read_file","arguments":"{}"}}]},
			{"role":"tool","call_id":"call_2","content":"file"}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	if got := gjson.GetBytes(out, "messages.1.tool_call_id").String(); got != "call_1" {
		t.Fatalf("messages.1.tool_call_id = %q, want %q", got, "call_1")
	}
	if got := gjson.GetBytes(out, "messages.3.tool_call_id").String(); got != "call_2" {
		t.Fatalf("messages.3.tool_call_id = %q, want %q", got, "call_2")
	}
	// messages.2 is a tool-call turn with no reasoning and no own content. We must
	// NOT copy message[0]'s "r1" onto it (that mismatched reasoning with the wrong
	// action and produced confused downstream reasoning). It gets reasoning
	// synthesised from its OWN tool call (read_file) instead.
	wantSynth := "I'll use the read_file tool to make progress on the task."
	if got := gjson.GetBytes(out, "messages.2.reasoning_content").String(); got != wantSynth {
		t.Fatalf("messages.2.reasoning_content = %q, want %q", got, wantSynth)
	}
	// message[0]'s own reasoning must be left intact.
	if got := gjson.GetBytes(out, "messages.0.reasoning_content").String(); got != "r1" {
		t.Fatalf("messages.0.reasoning_content = %q, want %q", got, "r1")
	}
}

func TestNormalizeKimiToolMessageLinks_DropsEmptyAssistantWithoutToolLink(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":"start"},
			{"role":"assistant","content":""},
			{"role":"assistant","content":"   "},
			{"role":"assistant","content":"","tool_calls":null},
			{"role":"assistant","content":[{"type":"text","text":"  "}]},
			{"role":"assistant"},
			{"role":"assistant","content":"keep"},
			{"role":"user","content":"next"}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 3 {
		t.Fatalf("messages length = %d, want 3, raw = %s", len(messages), gjson.GetBytes(out, "messages").Raw)
	}
	if got := messages[0].Get("content").String(); got != "start" {
		t.Fatalf("messages.0.content = %q, want %q", got, "start")
	}
	if got := messages[1].Get("content").String(); got != "keep" {
		t.Fatalf("messages.1.content = %q, want %q", got, "keep")
	}
	if got := messages[2].Get("content").String(); got != "next" {
		t.Fatalf("messages.2.content = %q, want %q", got, "next")
	}
}

func TestNormalizeKimiToolMessageLinks_PreservesAssistantWithToolLinkOrReasoning(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]},
			{"role":"assistant","content":"","function_call":{"name":"legacy_call","arguments":"{}"}},
			{"role":"assistant","content":"","reasoning_content":"thought"},
			{"role":"assistant","content":[{"type":"text","text":" visible "}]}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 4 {
		t.Fatalf("messages length = %d, want 4, raw = %s", len(messages), gjson.GetBytes(out, "messages").Raw)
	}
	if !messages[0].Get("tool_calls").Exists() {
		t.Fatalf("messages.0.tool_calls should exist")
	}
	if !messages[1].Get("function_call").Exists() {
		t.Fatalf("messages.1.function_call should exist")
	}
	if got := messages[2].Get("reasoning_content").String(); got != "thought" {
		t.Fatalf("messages.2.reasoning_content = %q, want %q", got, "thought")
	}
	if got := messages[3].Get("content.0.text").String(); got != " visible " {
		t.Fatalf("messages.3.content.0.text = %q, want %q", got, " visible ")
	}
}
