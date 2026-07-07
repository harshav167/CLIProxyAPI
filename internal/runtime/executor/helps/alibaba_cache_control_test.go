package helps

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const alibabaTokenPlanBaseURL = "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1"

// applyAlibaba runs the helper and returns the result, failing the test if it
// panics (so invalid-JSON / missing-field cases surface as test failures, not
// silent passes).
func applyAlibaba(t *testing.T, payload []byte, baseURL string) (out []byte) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ApplyAlibabaExplicitCache panicked for baseURL=%s: %v", baseURL, r)
		}
	}()
	return ApplyAlibabaExplicitCache(payload, baseURL)
}

func TestApplyAlibabaExplicitCache_NoopForNonAlibabaBaseURLs(t *testing.T) {
	bodies := [][]byte{
		[]byte(`{"messages":[{"role":"system","content":"X"},{"role":"user","content":"hi"}]}`),
		[]byte(`{"messages":[{"role":"system","content":"X"},{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`),
	}
	nonAlibaba := []string{
		"https://api.z.ai/api/coding/paas/v4",
		"https://api.fireworks.ai/inference/v1/",
		"https://ollama.com/v1",
		"https://api.gmi-serving.com/v1",
		"https://api.cline.bot/api/v1",
		"https://api.gpt.ge/v1",
		"https://integrate.api.nvidia.com/v1/",
		"https://api.apikey.fun/v1",
		"https://ava.ecorp.cc/v1",
	}
	for _, baseURL := range nonAlibaba {
		for _, body := range bodies {
			out := applyAlibaba(t, body, baseURL)
			if string(out) != string(body) {
				t.Fatalf("expected no-op for non-Alibaba baseURL %s; got %s", baseURL, out)
			}
		}
	}
}

func TestApplyAlibabaExplicitCache_StringSystemContentBecomesArrayWithCacheControl(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"You are GLM"},{"role":"user","content":"hi"}]}`)
	out := applyAlibaba(t, body, alibabaTokenPlanBaseURL)
	sys := gjson.GetBytes(out, "messages.0.content")
	if !sys.IsArray() {
		t.Fatalf("expected system content to be array; got %s", sys.Raw)
	}
	arr := sys.Array()
	if len(arr) != 1 {
		t.Fatalf("expected 1 block; got %d (%s)", len(arr), sys.Raw)
	}
	if got := arr[0].Get("type").String(); got != "text" {
		t.Fatalf("expected block type=text; got %q (%s)", got, sys.Raw)
	}
	if got := arr[0].Get("text").String(); got != "You are GLM" {
		t.Fatalf("expected text preserved; got %q (%s)", got, sys.Raw)
	}
	cc := arr[0].Get("cache_control")
	if !cc.Exists() {
		t.Fatalf("expected cache_control on system block; got %s", sys.Raw)
	}
	if got := cc.Get("type").String(); got != "ephemeral" {
		t.Fatalf("expected cache_control.type=ephemeral; got %q (%s)", got, sys.Raw)
	}
	if cc.Get("ttl").Exists() {
		t.Fatalf("Alibaba has no TTL option; ttl must be absent; got %s", sys.Raw)
	}
	if cc.Get("scope").Exists() {
		t.Fatalf("Alibaba has no scope concept; scope must be absent; got %s", sys.Raw)
	}
}

func TestApplyAlibabaExplicitCache_ArraySystemContentGetsMarkerOnLastBlock(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":[{"type":"text","text":"A"},{"type":"text","text":"B"}]},{"role":"user","content":"hi"}]}`)
	out := applyAlibaba(t, body, alibabaTokenPlanBaseURL)
	arr := gjson.GetBytes(out, "messages.0.content").Array()
	if len(arr) != 2 {
		t.Fatalf("expected 2 blocks preserved; got %d", len(arr))
	}
	if arr[0].Get("cache_control").Exists() {
		t.Fatalf("first block must NOT get marker; got %s", arr[0].Raw)
	}
	if !arr[1].Get("cache_control").Exists() {
		t.Fatalf("last block must get marker; got %s", arr[1].Raw)
	}
	if got := arr[1].Get("cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("expected ephemeral; got %q", got)
	}
}

func TestApplyAlibabaExplicitCache_Idempotent(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"u1"},{"role":"assistant","content":"a1"},{"role":"user","content":"u2"}]}`),
		[]byte(`{"messages":[{"role":"system","content":[{"type":"text","text":"S"}]},{"role":"user","content":"u1"},{"role":"assistant","content":"a1"},{"role":"user","content":"u2"}]}`),
	}
	for i, body := range cases {
		once := applyAlibaba(t, body, alibabaTokenPlanBaseURL)
		twice := applyAlibaba(t, once, alibabaTokenPlanBaseURL)
		if string(once) != string(twice) {
			t.Fatalf("case %d: f(f(x)) != f(x)\n once=%s\ntwice=%s", i, once, twice)
		}
	}
}

func TestApplyAlibabaExplicitCache_SingleTurnOnlySystemMarker(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"only turn"}]}`)
	out := applyAlibaba(t, body, alibabaTokenPlanBaseURL)
	if !gjson.GetBytes(out, "messages.0.content.0.cache_control").Exists() {
		t.Fatalf("system must get marker; got %s", out)
	}
	// Only 1 user message -> no second-to-last -> last user must NOT get a marker.
	lastUser := gjson.GetBytes(out, "messages.1.content")
	if lastUser.IsArray() {
		t.Fatalf("single-turn: last user content must stay string (no marker); got %s", lastUser.Raw)
	}
}

func TestApplyAlibabaExplicitCache_MultiTurnSecondMarkerOnSecondToLastUser(t *testing.T) {
	// 1 system + user1 + assistant1 + user2 (current turn). Marker 2 must land
	// on user1 (the previous turn boundary), NOT user2 (current turn).
	body := []byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"u1"},{"role":"assistant","content":"a1"},{"role":"user","content":"u2"}]}`)
	out := applyAlibaba(t, body, alibabaTokenPlanBaseURL)
	// user1 (messages.1) gets marker
	u1 := gjson.GetBytes(out, "messages.1.content")
	if !u1.IsArray() {
		t.Fatalf("user1 content must be array-with-marker; got %s", u1.Raw)
	}
	if !u1.Array()[0].Get("cache_control").Exists() {
		t.Fatalf("user1 first block must have marker; got %s", u1.Raw)
	}
	if got := u1.Array()[0].Get("cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("expected ephemeral on user1; got %q", got)
	}
	// user2 (messages.3, current turn) must NOT get a marker
	u2 := gjson.GetBytes(out, "messages.3.content")
	if u2.IsArray() {
		t.Fatalf("current-turn user2 must stay string (no marker); got %s", u2.Raw)
	}
}

func TestApplyAlibabaExplicitCache_TurnWithToolCallHasNoSecondToLastUser(t *testing.T) {
	// Single turn with a tool call: system + user1 + assistant(tool_calls) + tool.
	// Only ONE user message exists, so there is no second-to-last user to anchor
	// marker 2 on. Only the system marker fires. This is correct: a tool-call
	// turn with a single prior user message has no prior turn boundary to cache.
	body := []byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"u1"},{"role":"assistant","content":"a1","tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","content":"result","name":"f","tool_call_id":"t1"}]}`)
	out := applyAlibaba(t, body, alibabaTokenPlanBaseURL)
	// system marker present
	if !gjson.GetBytes(out, "messages.0.content.0.cache_control").Exists() {
		t.Fatalf("system must get marker; got %s", out)
	}
	// user1 stays string (only 1 user -> no second-to-last)
	u1 := gjson.GetBytes(out, "messages.1.content")
	if u1.IsArray() {
		t.Fatalf("single-user tool-call turn: user1 must stay string; got %s", u1.Raw)
	}
	// tool result stays string
	tool := gjson.GetBytes(out, "messages.3.content")
	if tool.IsArray() {
		t.Fatalf("tool result must stay string; got %s", tool.Raw)
	}
}

func TestApplyAlibabaExplicitCache_MultiTurnToolFlowMarkerOnSecondToLastUser(t *testing.T) {
	// Two real turns where the first turn used a tool:
	//   system + user1 + assistant(tool_calls) + tool + assistant(final) + user2(current)
	// user1 is the second-to-last user -> marker 2 lands on user1.
	body := []byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"u1"},{"role":"assistant","content":"a1","tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","content":"result","name":"f","tool_call_id":"t1"},{"role":"assistant","content":"final answer"},{"role":"user","content":"u2 current turn"}]}`)
	out := applyAlibaba(t, body, alibabaTokenPlanBaseURL)
	u1 := gjson.GetBytes(out, "messages.1.content")
	if !u1.IsArray() || !u1.Array()[0].Get("cache_control").Exists() {
		t.Fatalf("user1 (second-to-last user) must get marker; got %s", u1.Raw)
	}
	// current turn user2 stays string
	u2 := gjson.GetBytes(out, "messages.5.content")
	if u2.IsArray() {
		t.Fatalf("current-turn user2 must stay string; got %s", u2.Raw)
	}
}

func TestApplyAlibabaExplicitCache_NoMarkerOnAssistantLastMessage(t *testing.T) {
	// Only one user (current turn) then assistant. No second-to-last user, so
	// only the system marker fires.
	body := []byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"u"},{"role":"assistant","content":"a"}]}`)
	out := applyAlibaba(t, body, alibabaTokenPlanBaseURL)
	if !gjson.GetBytes(out, "messages.0.content.0.cache_control").Exists() {
		t.Fatalf("system must get marker; got %s", out)
	}
	assistant := gjson.GetBytes(out, "messages.2.content")
	if assistant.IsArray() {
		t.Fatalf("assistant must stay string; got %s", assistant.Raw)
	}
}

func TestApplyAlibabaExplicitCache_SkipsWhenClientAlreadySentCacheControl(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":[{"type":"text","text":"S","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":"u1"},{"role":"assistant","content":"a1"},{"role":"user","content":"u2"}]}`)
	out := applyAlibaba(t, body, alibabaTokenPlanBaseURL)
	if string(out) != string(body) {
		t.Fatalf("client already sent cache_control -> must be no-op; got %s", out)
	}
}

func TestApplyAlibabaExplicitCache_MissingSystemMessageIsNoop(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out := applyAlibaba(t, body, alibabaTokenPlanBaseURL)
	if string(out) != string(body) {
		t.Fatalf("missing system message -> must be no-op; got %s", out)
	}
}

func TestApplyAlibabaExplicitCache_InvalidJSONIsNoop(t *testing.T) {
	body := []byte(`{not valid json`)
	out := applyAlibaba(t, body, alibabaTokenPlanBaseURL)
	if string(out) != string(body) {
		t.Fatalf("invalid JSON -> must be no-op; got %s", out)
	}
}

func TestApplyAlibabaExplicitCache_JSONRoundTripsCleanly(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"u1"},{"role":"assistant","content":"a1"},{"role":"user","content":"u2"}]}`)
	out := applyAlibaba(t, body, alibabaTokenPlanBaseURL)
	if !gjson.ValidBytes(out) {
		t.Fatalf("output must be valid JSON; got %s", out)
	}
}

func TestApplyAlibabaExplicitCache_AtMostTwoMarkers(t *testing.T) {
	// 50-message conversation: 1 system + 24 (user/assistant pairs) + 1 current user.
	// Exactly 2 markers expected: system + second-to-last user.
	var sb strings.Builder
	sb.WriteString(`{"messages":[{"role":"system","content":"S"}`)
	for i := 0; i < 24; i++ {
		sb.WriteString(`,{"role":"user","content":"u`)
		sb.WriteString(itoa(i))
		sb.WriteString(`"},{"role":"assistant","content":"a`)
		sb.WriteString(itoa(i))
		sb.WriteString(`"}`)
	}
	sb.WriteString(`,{"role":"user","content":"current"}]}`)
	body := []byte(sb.String())
	out := applyAlibaba(t, body, alibabaTokenPlanBaseURL)
	count := 0
	messages := gjson.GetBytes(out, "messages")
	messages.ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if content.IsArray() {
			content.ForEach(func(_, item gjson.Result) bool {
				if item.Get("cache_control").Exists() {
					count++
				}
				return true
			})
		}
		return true
	})
	if count != 2 {
		t.Fatalf("expected exactly 2 markers; got %d", count)
	}
}

// itoa is a tiny int->string helper to avoid pulling in strconv for one loop.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
