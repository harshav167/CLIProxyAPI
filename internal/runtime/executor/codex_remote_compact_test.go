package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// TestSynthesizeChatCompletionsErrorChunkRealUpstreamFailure verifies that a
// real Codex Responses-API response.failed event gets converted to the OpenAI
// streaming error envelope. Cursor and openai-node both expect this shape on
// terminal failure; forwarding the raw Responses event would land as
// `data: {"type":"response.failed",...}` and break their chunk parsers.
func TestSynthesizeChatCompletionsErrorChunkRealUpstreamFailure(t *testing.T) {
	upstream := []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again."},"model":"gpt-5.5"}}`)
	out := synthesizeChatCompletionsErrorChunk(upstream)

	if got := gjson.GetBytes(out, "error.code").String(); got != "context_length_exceeded" {
		t.Errorf("error.code = %q, want context_length_exceeded", got)
	}
	if got := gjson.GetBytes(out, "error.message").String(); !strings.Contains(got, "exceeds the context window") {
		t.Errorf("error.message = %q, want substring 'exceeds the context window'", got)
	}
	if got := gjson.GetBytes(out, "error.type").String(); got != "server_error" {
		t.Errorf("error.type = %q, want server_error", got)
	}
	// Confirm we don't leak the original Responses-API discriminant.
	if got := gjson.GetBytes(out, "type").String(); got != "" {
		t.Errorf("output should not carry response.type; got %q", got)
	}
}

// TestSynthesizeChatCompletionsErrorChunkSyntheticFailure verifies the
// synthesized-from-emitSyntheticFailure shape (proxy_-prefixed code, message
// is the raw error string). Even though synthetics are now suppressed for
// chat-completions clients, the helper itself should still produce a valid
// envelope when called directly.
func TestSynthesizeChatCompletionsErrorChunkSyntheticFailure(t *testing.T) {
	synthetic := []byte(`{"type":"response.failed","response":{"id":"resp_proxy_synth","object":"response","status":"failed","error":{"code":"proxy_upstream_read_error","message":"read tcp 1.2.3.4:443: i/o timeout"}}}`)
	out := synthesizeChatCompletionsErrorChunk(synthetic)

	if got := gjson.GetBytes(out, "error.code").String(); got != "proxy_upstream_read_error" {
		t.Errorf("error.code = %q, want proxy_upstream_read_error", got)
	}
	if got := gjson.GetBytes(out, "error.type").String(); got != "server_error" {
		t.Errorf("error.type = %q, want server_error", got)
	}
}

// TestSynthesizeChatCompletionsErrorChunkFallbacks verifies graceful defaults
// when the upstream payload is malformed or missing fields.
func TestSynthesizeChatCompletionsErrorChunkFallbacks(t *testing.T) {
	cases := []struct {
		name       string
		payload    string
		wantCode   string
		wantMsgSub string
	}{
		{
			name:       "completely empty payload",
			payload:    `{}`,
			wantCode:   "upstream_error",
			wantMsgSub: "upstream stream ended",
		},
		{
			name:       "top-level error field (not nested under response)",
			payload:    `{"error":{"code":"bad_request","message":"missing field"}}`,
			wantCode:   "bad_request",
			wantMsgSub: "missing field",
		},
		{
			name:       "response.error.message but no code",
			payload:    `{"response":{"error":{"message":"something broke"}}}`,
			wantCode:   "upstream_error",
			wantMsgSub: "something broke",
		},
		{
			name:       "response.error.code but no message",
			payload:    `{"response":{"error":{"code":"timeout"}}}`,
			wantCode:   "timeout",
			wantMsgSub: "upstream stream ended",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := synthesizeChatCompletionsErrorChunk([]byte(tc.payload))
			if got := gjson.GetBytes(out, "error.code").String(); got != tc.wantCode {
				t.Errorf("error.code = %q, want %q", got, tc.wantCode)
			}
			if got := gjson.GetBytes(out, "error.message").String(); !strings.Contains(got, tc.wantMsgSub) {
				t.Errorf("error.message = %q, want substring %q", got, tc.wantMsgSub)
			}
		})
	}
}

// TestNeedsRawFallbackWhitelist confirms the gate only fires for terminal
// failure events the translator has no rule for. Bookkeeping events
// (response.created, response.in_progress, response.output_text.done,
// response.content_part.done, etc.) MUST drop silently — forwarding them
// raw to chat-completions clients would break parsers.
func TestNeedsRawFallbackWhitelist(t *testing.T) {
	wantTrue := []string{"response.failed", "response.error"}
	wantFalse := []string{
		"response.created",
		"response.in_progress",
		"response.completed",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.added",
		"response.content_part.done",
		"response.output_item.added",
		"response.output_item.done",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"codex.rate_limits", // backend-only signal
		"",                  // unknown / unparseable
		"some.future.event",
	}
	for _, et := range wantTrue {
		if !needsRawFallback(et) {
			t.Errorf("needsRawFallback(%q) = false, want true (terminal failure must surface)", et)
		}
	}
	for _, et := range wantFalse {
		if needsRawFallback(et) {
			t.Errorf("needsRawFallback(%q) = true, want false (forwarding raw breaks chat-completions parsers)", et)
		}
	}
}

// TestAdjustKeepTailForPairsKeepsToolCallPairs verifies the boundary-grow
// logic prevents a function_call from being summarized while its matching
// function_call_output remains in the retained tail (or vice versa).
// Responses-API rejects rewritten input where one half of a tool-call pair
// is missing.
func TestAdjustKeepTailForPairsKeepsToolCallPairs(t *testing.T) {
	// Layout (10 items):
	//   [0..1]  user / system (head)
	//   [2..3]  user / function_call call_id=A
	//   [4]     function_call_output for A    ← if compacted alone, A is split
	//   [5..6]  assistant message / user
	//   [7]     function_call call_id=B
	//   [8]     function_call_output for B
	//   [9]     assistant message
	items := mustParseJSONArray(t, `[
		{"type":"message","role":"user","content":"hello"},
		{"type":"message","role":"assistant","content":"hi"},
		{"type":"message","role":"user","content":"do thing"},
		{"type":"function_call","call_id":"A","name":"Read","arguments":"{}"},
		{"type":"function_call_output","call_id":"A","output":"file contents"},
		{"type":"message","role":"assistant","content":"got it"},
		{"type":"message","role":"user","content":"now another"},
		{"type":"function_call","call_id":"B","name":"Write","arguments":"{}"},
		{"type":"function_call_output","call_id":"B","output":"ok"},
		{"type":"message","role":"assistant","content":"done"}
	]`)

	// Initial: keepHead=2, keepTail=3 → middle=[2..6], tail starts at items[7].
	// Items[7] is function_call B, items[8] is its output. Both in tail → pair safe.
	// But items[4] is function_call_output A; items[3] is function_call A.
	// items[3] is in middle (would be summarized), items[4] is also in middle.
	// Pair fully in middle → pair-safe. No expansion needed for this layout.
	keepHead := 2
	keepTail := 3
	got := adjustKeepTailForPairs(items, keepHead, keepTail)
	if got < keepTail {
		t.Errorf("adjustKeepTailForPairs(2,3) = %d, want >= %d (must not shrink)", got, keepTail)
	}

	// Now stress: keepHead=2, keepTail=2 → tail=[items[8], items[9]]. items[8]
	// is function_call_output B; its function_call B is items[7] in middle.
	// Boundary splits the pair → adjuster MUST grow keepTail to include items[7].
	got = adjustKeepTailForPairs(items, 2, 2)
	if got < 3 {
		t.Errorf("adjustKeepTailForPairs(2,2) = %d, want >= 3 (must grow keepTail to include the function_call matching the orphaned output)", got)
	}
}

// TestCompactCooldownLifecycle exercises the full markCompactFailure →
// compactInCooldown → expiry → compactCooldownPrune path.
func TestCompactCooldownLifecycle(t *testing.T) {
	// Use a unique session/auth pair to isolate from any other test state.
	const sessionKey = "test-cooldown-session-xyz123"
	const authID = "test-auth-xyz123"

	// Pre-condition: not in cooldown.
	if compactInCooldown(sessionKey, authID) {
		t.Fatalf("pre-condition: %s|%s should not be in cooldown", sessionKey, authID)
	}

	// Mark a failure → in cooldown.
	markCompactFailure(sessionKey, authID)
	if !compactInCooldown(sessionKey, authID) {
		t.Fatalf("after markCompactFailure: %s|%s should be in cooldown", sessionKey, authID)
	}

	// Force the entry to look expired by rewriting the timestamp directly.
	// (Real expiry takes 30s — we shortcut for test speed.)
	compactCooldownMu.Lock()
	compactCooldown[sessionKey+"|"+authID] = time.Now().Add(-2 * remoteCompactFailureCooldown)
	compactCooldownMu.Unlock()

	// Lazy eviction via lookup — should report not-in-cooldown AND remove.
	if compactInCooldown(sessionKey, authID) {
		t.Fatalf("expired entry should report not-in-cooldown and self-evict")
	}
	compactCooldownMu.Lock()
	_, exists := compactCooldown[sessionKey+"|"+authID]
	compactCooldownMu.Unlock()
	if exists {
		t.Fatalf("compactInCooldown should have lazily deleted the expired entry")
	}

	// Background pruner: insert another expired entry and verify prune removes it
	// without a lookup happening.
	const orphanKey = "test-orphan-session|test-orphan-auth"
	compactCooldownMu.Lock()
	compactCooldown[orphanKey] = time.Now().Add(-2 * remoteCompactFailureCooldown)
	compactCooldownMu.Unlock()

	compactCooldownPrune()

	compactCooldownMu.Lock()
	_, stillThere := compactCooldown[orphanKey]
	compactCooldownMu.Unlock()
	if stillThere {
		t.Fatalf("compactCooldownPrune should remove orphaned expired entries that no lookup would revisit")
	}
}

// TestCompactInCooldownEmptySessionKey confirms the helper short-circuits
// when given an empty session key (defensive — empty key shouldn't ever
// be a valid cooldown subject).
func TestCompactInCooldownEmptySessionKey(t *testing.T) {
	if compactInCooldown("", "any-auth") {
		t.Fatal("compactInCooldown(\"\", _) must return false")
	}
	// markCompactFailure with empty key should also be a no-op (no panic, no entry).
	before := mapLen(compactCooldown)
	markCompactFailure("", "any-auth")
	after := mapLen(compactCooldown)
	if after != before {
		t.Fatalf("markCompactFailure(\"\", _) created entry: before=%d after=%d", before, after)
	}
}

// mustParseJSONArray helps tests build []gjson.Result inputs from a literal
// array. Failing the test on parse error keeps the test code readable.
func mustParseJSONArray(t *testing.T, raw string) []gjson.Result {
	t.Helper()
	res := gjson.Parse(raw)
	if !res.IsArray() {
		t.Fatalf("test fixture is not a JSON array: %s", raw)
	}
	return res.Array()
}

// mapLen returns len(m) under the cooldown mutex (caller must NOT hold it).
func mapLen(m map[string]time.Time) int {
	compactCooldownMu.Lock()
	defer compactCooldownMu.Unlock()
	return len(m)
}

// TestBridgeSessionKeyRejectsSyntheticPrefix is the regression test for the
// High-severity collision fix: bridgeSessionKey() must NOT use synthetic
// "cli-proxy-" keys for stateful WS-bridge routing. Synthetic keys are
// content-derived (sha256 of model + first user message) and would collide
// across distinct chats with the same opening prompt — using them as bridge
// session IDs would route chat B's request to chat A's previous_response_id
// chain upstream → cross-conversation context leakage.
//
// The synthetic key still flows upstream as prompt_cache_key in the request
// body (the inject helper's actual purpose — server-side prompt cache
// benefit, where same content sharing a cache is the desired behavior).
func TestBridgeSessionKeyRejectsSyntheticPrefix(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string // expected return; "" means "bridge skipped"
	}{
		{
			name:    "synthetic cli-proxy- key → rejected (bridge skipped)",
			payload: `{"prompt_cache_key":"cli-proxy-c5e144e6850fb66b513b002f552aa985"}`,
			want:    "",
		},
		{
			name:    "real Cursor UUID-style key → accepted (bridge routes)",
			payload: `{"prompt_cache_key":"d3498f66-5fae-5e1e-9b81-81de4bb1441a"}`,
			want:    "d3498f66-5fae-5e1e-9b81-81de4bb1441a",
		},
		{
			name:    "Droid-style session key → accepted (bridge routes)",
			payload: `{"prompt_cache_key":"droid-session-abc-123"}`,
			want:    "droid-session-abc-123",
		},
		{
			name:    "no prompt_cache_key at all → empty (bridge skipped, expected)",
			payload: `{"model":"gpt-5.5"}`,
			want:    "",
		},
		{
			name:    "empty string prompt_cache_key → empty (bridge skipped)",
			payload: `{"prompt_cache_key":""}`,
			want:    "",
		},
		{
			name:    "key starting with literal 'cli-proxy' but no dash → accepted (only the exact prefix triggers rejection)",
			payload: `{"prompt_cache_key":"cli-proxy"}`,
			want:    "cli-proxy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bridgeSessionKey(cliproxyexecutor.Options{}, []byte(tc.payload))
			if got != tc.want {
				t.Errorf("bridgeSessionKey(%s) = %q, want %q", tc.payload, got, tc.want)
			}
		})
	}
}

// TestUnsupportedEventWarnDedupeAndExpiry verifies the bounded warn-once
// helper: first call for a (session, eventType) pair returns true (caller
// warns), subsequent calls within the TTL window return false (caller
// debug-logs), and the periodic pruner evicts entries past the TTL so the
// underlying map can't grow unboundedly across long process lifetimes.
func TestUnsupportedEventWarnDedupeAndExpiry(t *testing.T) {
	const sess = "test-warn-session-aaa"
	const evt = "response.unknown.event"

	// First call → emit warn.
	if !recordUnsupportedEventWarn(sess, evt) {
		t.Fatal("first call must return true (warn-eligible)")
	}
	// Second call within TTL → suppress.
	if recordUnsupportedEventWarn(sess, evt) {
		t.Fatal("second call within TTL must return false (already-warned)")
	}
	// Different sessionID, same eventType → still warn-eligible (per-session dedupe).
	if !recordUnsupportedEventWarn("test-warn-session-bbb", evt) {
		t.Fatal("different session must be warn-eligible")
	}
	// Different eventType, same session → still warn-eligible (per-event dedupe).
	if !recordUnsupportedEventWarn(sess, "response.different.event") {
		t.Fatal("different eventType must be warn-eligible")
	}

	// Force the original (sess, evt) entry to look expired.
	unsupportedEventWarnSeenMu.Lock()
	unsupportedEventWarnSeen[sess+"\x00"+evt] = time.Now().Add(-2 * unsupportedEventWarnTTL)
	unsupportedEventWarnSeenMu.Unlock()

	// Pruner should evict the expired entry without a lookup.
	pruneUnsupportedEventWarnSeen()
	unsupportedEventWarnSeenMu.Lock()
	_, stillThere := unsupportedEventWarnSeen[sess+"\x00"+evt]
	unsupportedEventWarnSeenMu.Unlock()
	if stillThere {
		t.Fatal("pruneUnsupportedEventWarnSeen must remove entries past TTL")
	}

	// After eviction, the same (session, eventType) pair is warn-eligible
	// again — TTL expiry resets the suppression.
	if !recordUnsupportedEventWarn(sess, evt) {
		t.Fatal("after TTL expiry + prune, same pair must be warn-eligible again")
	}
}

// TestShouldStartCursorKeepaliveGates confirms all three gates work together:
// config flag enabled, source format is openai (chat-completions), and the
// client UA identifies as Cursor. Any gate failing returns false so non-Cursor
// or Responses-native clients never get keepalives.
func TestShouldStartCursorKeepaliveGates(t *testing.T) {
	cursorCtx := WithClientUserAgent(context.Background(), "Cursor/1.0")
	droidCtx := WithClientUserAgent(context.Background(), "factory-cli/0.108.0")
	enabledCfg := &config.Config{
		SDKConfig: config.SDKConfig{},
	}
	enabledCfg.CursorKeepalive.Enabled = true
	disabledCfg := &config.Config{}

	openai := sdktranslator.FromString("openai")
	openaiResponse := sdktranslator.FromString("openai-response")

	cases := []struct {
		name string
		ctx  context.Context
		cfg  *config.Config
		from sdktranslator.Format
		want bool
	}{
		{"all gates pass: cursor + openai + enabled", cursorCtx, enabledCfg, openai, true},
		{"config disabled → false even with cursor + openai", cursorCtx, disabledCfg, openai, false},
		{"nil config → false (defensive)", cursorCtx, nil, openai, false},
		{"droid UA → false (only cursor needs this)", droidCtx, enabledCfg, openai, false},
		{"openai-response source → false (responses-native clients handle their own)", cursorCtx, enabledCfg, openaiResponse, false},
		{"empty UA → false (defaults to droid-protective IsDroidClient)", context.Background(), enabledCfg, openai, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldStartCursorKeepalive(tc.ctx, tc.cfg, tc.from)
			if got != tc.want {
				t.Errorf("shouldStartCursorKeepalive = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCursorKeepaliveIntervalDefault confirms the fallback applies when
// IntervalMs is unset or non-positive.
func TestCursorKeepaliveIntervalDefault(t *testing.T) {
	cases := []struct {
		name       string
		intervalMs int
		want       time.Duration
	}{
		{"unset (zero) → default 1500ms", 0, cursorKeepaliveDefaultInterval},
		{"negative → default 1500ms", -100, cursorKeepaliveDefaultInterval},
		{"explicit 500ms → 500ms", 500, 500 * time.Millisecond},
		{"explicit 5s → 5s", 5000, 5 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.CursorKeepalive.IntervalMs = tc.intervalMs
			got := cursorKeepaliveInterval(cfg)
			if got != tc.want {
				t.Errorf("cursorKeepaliveInterval = %v, want %v", got, tc.want)
			}
		})
	}
	if got := cursorKeepaliveInterval(nil); got != cursorKeepaliveDefaultInterval {
		t.Errorf("cursorKeepaliveInterval(nil) = %v, want %v", got, cursorKeepaliveDefaultInterval)
	}
}

// TestRunCursorKeepaliveStopsOnSignal verifies the goroutine exits cleanly
// when its stop channel closes — covers the "first content event arrived"
// case where the read loop signals "no more keepalives needed".
func TestRunCursorKeepaliveStopsOnSignal(t *testing.T) {
	emitted := 0
	send := func(c cliproxyexecutor.StreamChunk) bool {
		emitted++
		return true
	}
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		runCursorKeepalive(context.Background(), send, [][]byte{[]byte(`{"id":"chatcmpl-test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{}}]}`)}, 50*time.Millisecond, stop, "test-session")
		close(done)
	}()

	// Let it tick a few times, then stop.
	time.Sleep(180 * time.Millisecond) // ~3 ticks
	close(stop)

	select {
	case <-done:
		// Goroutine returned cleanly.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runCursorKeepalive did not exit within 500ms of stop signal")
	}
	if emitted < 2 || emitted > 5 {
		t.Errorf("emitted %d keepalives in ~180ms with 50ms interval; expected 2-5", emitted)
	}
}

// TestRunCursorKeepaliveStopsOnContextDone verifies the goroutine exits
// when the request context is canceled (e.g. client disconnect).
func TestRunCursorKeepaliveStopsOnContextDone(t *testing.T) {
	send := func(c cliproxyexecutor.StreamChunk) bool { return true }
	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan struct{})
	defer close(stop)
	done := make(chan struct{})

	go func() {
		runCursorKeepalive(ctx, send, [][]byte{[]byte(`{}`)}, 50*time.Millisecond, stop, "test-session")
		close(done)
	}()
	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runCursorKeepalive did not exit within 500ms of ctx cancel")
	}
}

// TestRunCursorKeepaliveSkipsWhenNoChunks verifies the keepalive returns
// immediately if cachedChunks is empty — there's no client-safe shape to
// emit, so emitting raw Codex payload would risk breaking chat.completion.chunk
// parsers (the High reviewer fix).
func TestRunCursorKeepaliveSkipsWhenNoChunks(t *testing.T) {
	called := false
	send := func(c cliproxyexecutor.StreamChunk) bool {
		called = true
		return true
	}
	stop := make(chan struct{})
	defer close(stop)
	done := make(chan struct{})

	go func() {
		runCursorKeepalive(context.Background(), send, nil, 30*time.Millisecond, stop, "test-session")
		close(done)
	}()

	// Wait long enough that any keepalive ticks would have fired.
	time.Sleep(100 * time.Millisecond)

	select {
	case <-done:
		// Goroutine returned promptly without emitting.
	default:
		t.Fatal("runCursorKeepalive should return immediately when cachedChunks is nil/empty")
	}
	if called {
		t.Fatal("runCursorKeepalive must not call send when cachedChunks is empty (would risk emitting wrong shape)")
	}
}

// TestRunCursorKeepaliveEmitsAllChunksPerTick verifies multi-chunk
// re-emission. If the translator returned 3 chunks for response.in_progress,
// each tick should re-emit all 3 (preserving the original event boundaries).
func TestRunCursorKeepaliveEmitsAllChunksPerTick(t *testing.T) {
	emitted := 0
	send := func(c cliproxyexecutor.StreamChunk) bool {
		emitted++
		return true
	}
	stop := make(chan struct{})
	done := make(chan struct{})

	chunks := [][]byte{
		[]byte(`{"chunk":1}`),
		[]byte(`{"chunk":2}`),
		[]byte(`{"chunk":3}`),
	}
	go func() {
		runCursorKeepalive(context.Background(), send, chunks, 50*time.Millisecond, stop, "test-session")
		close(done)
	}()

	// Let ~3 ticks fire.
	time.Sleep(180 * time.Millisecond)
	close(stop)

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runCursorKeepalive did not exit within 500ms of stop")
	}

	// Per tick we send 3 chunks. ~3 ticks → ~9 emissions. Allow loose bounds for scheduler jitter.
	if emitted < 6 || emitted > 12 {
		t.Errorf("emitted %d chunks in ~180ms with 50ms ticker × 3 chunks/tick; expected 6-12", emitted)
	}
	// Must always emit in multiples of 3 (whole-tick boundary on stop).
	if emitted%3 != 0 {
		t.Errorf("emitted %d chunks; expected multiple of 3 (chunks-per-tick)", emitted)
	}
}

// TestRunCursorKeepaliveStopsOnSendFailure verifies the goroutine exits
// when send() returns false (client closed the stream out from under us).
func TestRunCursorKeepaliveStopsOnSendFailure(t *testing.T) {
	send := func(c cliproxyexecutor.StreamChunk) bool { return false } // simulate disconnect
	stop := make(chan struct{})
	defer close(stop)
	done := make(chan struct{})

	go func() {
		runCursorKeepalive(context.Background(), send, [][]byte{[]byte(`{}`)}, 30*time.Millisecond, stop, "test-session")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runCursorKeepalive did not exit within 500ms after send failure")
	}
}

// TestBridgeSessionKeyPrefersExecutionSessionID confirms an explicit session
// ID from options always wins over whatever's in the payload — so a real
// session-aware client (e.g. Codex CLI passing ExecutionSessionID metadata)
// is never overridden by synthetic-key suppression logic.
func TestBridgeSessionKeyPrefersExecutionSessionID(t *testing.T) {
	opts := cliproxyexecutor.Options{
		Metadata: map[string]interface{}{
			cliproxyexecutor.ExecutionSessionMetadataKey: "explicit-session-from-options",
		},
	}
	// Even with a synthetic "cli-proxy-" key in the body, the explicit
	// session ID from opts MUST win — synthetic-key rejection is only the
	// fallback behavior, not an override.
	payload := []byte(`{"prompt_cache_key":"cli-proxy-aaaa"}`)
	got := bridgeSessionKey(opts, payload)
	if got != "explicit-session-from-options" {
		t.Errorf("bridgeSessionKey with explicit ExecutionSessionID = %q, want %q", got, "explicit-session-from-options")
	}
}
