package executor

import (
	"bytes"
	"testing"
)

func TestNormalizeSynthIDsRewritesIdFieldOnly(t *testing.T) {
	in := []byte(`{"type":"message","id":"msg_synth_42","content":"hi"}`)
	want := []byte(`{"type":"message","id":"msg_synth_X","content":"hi"}`)
	if got := normalizeSynthIDs(in); !bytes.Equal(got, want) {
		t.Fatalf("normalize = %s, want %s", got, want)
	}
}

func TestCanonicalizeFunctionCallStripsBackendFieldsAndNormalizesOrder(t *testing.T) {
	// Backend response.output form: includes id + status, arbitrary field order.
	backend := []byte(`{"id":"fc_0e7c25c754","type":"function_call","status":"completed","arguments":"{\"x\":1}","call_id":"call_abc","name":"Execute"}`)
	// Droid echo form: canonical order, no id/status.
	droid := []byte(`{"type":"function_call","call_id":"call_abc","name":"Execute","arguments":"{\"x\":1}"}`)
	canonBackend := canonicalizeBridgeItem(backend)
	canonDroid := canonicalizeBridgeItem(droid)
	if !bytes.Equal(canonBackend, canonDroid) {
		t.Fatalf("backend and droid forms must canonicalize equal:\n  backend -> %s\n  droid   -> %s", canonBackend, canonDroid)
	}
	// Canonical output should match droid's exact bytes since droid's form is already canonical.
	if !bytes.Equal(canonDroid, droid) {
		t.Fatalf("droid form already canonical; canon = %s", canonDroid)
	}
}

func TestCanonicalizeFunctionCallOutputPassthrough(t *testing.T) {
	in := []byte(`{"type":"function_call_output","call_id":"call_abc","output":"done"}`)
	if got := canonicalizeBridgeItem(in); !bytes.Equal(got, in) {
		t.Fatalf("function_call_output should passthrough; got %s", got)
	}
}

func TestCanonicalizeMessageInputFormWithoutType(t *testing.T) {
	// Droid's user-input message form has no `type` field.
	in := []byte(`{"role":"user","content":[{"type":"input_text","text":"hi"}]}`)
	want := []byte(`{"role":"user","content":[{"type":"input_text","text":"hi"}]}`)
	if got := canonicalizeBridgeItem(in); !bytes.Equal(got, want) {
		t.Fatalf("canonical = %s, want %s", got, want)
	}
}

func TestCanonicalizeMessageStripsStatusKeepsRealID(t *testing.T) {
	// Backend-returned assistant message: includes id + status; content blocks have annotations+logprobs.
	backend := []byte(`{"id":"msg_abc123","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":"hi"}]}`)
	// Client echo form: no id/status at top, content blocks stripped to {type,text}.
	client := []byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}],"id":"msg_abc123"}`)
	if got := canonicalizeBridgeItem(backend); !bytes.Equal(got, client) {
		t.Fatalf("backend canonical mismatch:\n  got:  %s\n  want: %s", got, client)
	}
}

func TestCanonicalizeMessageCollapsesSynthID(t *testing.T) {
	in := []byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"x"}],"id":"msg_synth_42"}`)
	want := []byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"x"}],"id":"msg_synth_X"}`)
	if got := canonicalizeBridgeItem(in); !bytes.Equal(got, want) {
		t.Fatalf("canon = %s, want %s", got, want)
	}
}

func TestCanonicalizeUnknownTypeFallsBackToSynthNormalization(t *testing.T) {
	// A type we don't map should pass through, but still normalize synth ids.
	in := []byte(`{"type":"novel_thing","id":"msg_synth_5","payload":"x"}`)
	want := []byte(`{"type":"novel_thing","id":"msg_synth_X","payload":"x"}`)
	if got := canonicalizeBridgeItem(in); !bytes.Equal(got, want) {
		t.Fatalf("canon = %s, want %s", got, want)
	}
}

func TestIsEventTypeSupportedByDroidKnownTypes(t *testing.T) {
	supported := []string{
		"response.created",
		"response.in_progress",
		"response.output_text.delta",
		"response.output_text.done",
		"response.output_item.added",
		"response.output_item.done",
		"response.completed",
		"response.failed",
		"response.error",
		"response.reasoning_summary_text.delta",
		"response.function_call_arguments.delta",
		"response.content_part.added",
		"response.custom_tool_call_input.delta",
	}
	for _, typ := range supported {
		if !IsEventTypeSupportedByDroid(typ) {
			t.Errorf("expected %q to be supported", typ)
		}
	}
}

func TestIsEventTypeSupportedByDroidRejectsKnownCrashers(t *testing.T) {
	// These types are emitted by the OpenAI backend but are NOT in the
	// Droid-bundled openai-node SDK's discriminated union; forwarding them
	// crashes the SDK with "undefined is not an object (evaluating 'C.type')".
	crashers := []string{
		"response.content_part.done",
		"response.done",
		"response.reasoning_text.done",
	}
	for _, typ := range crashers {
		if IsEventTypeSupportedByDroid(typ) {
			t.Errorf("expected %q to be REJECTED (not in Droid allowlist)", typ)
		}
	}
}

func TestIsEventTypeSupportedByDroidEmptyTypeForwarded(t *testing.T) {
	// Defensive: empty type (SSE comment, keepalive, parse miss) must forward,
	// not be dropped. The bridge's judgement on a missing field is unreliable.
	if !IsEventTypeSupportedByDroid("") {
		t.Fatal("empty event type must forward (defensive default)")
	}
}

func TestIsEventTypeSupportedByDroidRejectsFutureUnknowns(t *testing.T) {
	// Any hypothetical new event type we haven't harvested yet must be
	// rejected until the maintainer extends the allowlist.
	unknowns := []string{
		"response.web_search.delta",
		"response.image.generated",
		"response.made_up_event",
	}
	for _, typ := range unknowns {
		if IsEventTypeSupportedByDroid(typ) {
			t.Errorf("expected unknown type %q to be rejected until allowlist is extended", typ)
		}
	}
}

func TestCanonicalizeContentBlockStripsAnnotationsAndLogprobs(t *testing.T) {
	backend := []byte(`{"type":"output_text","annotations":[],"logprobs":[],"text":"hello world"}`)
	client := []byte(`{"type":"output_text","text":"hello world"}`)
	if got := canonicalizeContentBlock(backend); !bytes.Equal(got, client) {
		t.Fatalf("content block should strip annotations+logprobs:\n  got:  %s\n  want: %s", got, client)
	}
}

func TestCanonicalizeContentBlockPassthroughUnknownType(t *testing.T) {
	in := []byte(`{"type":"novel_block","data":"x","extra":true}`)
	if got := canonicalizeContentBlock(in); !bytes.Equal(got, in) {
		t.Fatalf("unknown content block type should passthrough: got %s", got)
	}
}

func TestCanonicalizeMessageWithContentBlockDrift(t *testing.T) {
	// Backend response.output message: has id, status, phase; content blocks have annotations+logprobs
	backend := []byte(`{"id":"msg_abc","type":"message","status":"completed","phase":"final_answer","role":"assistant","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":"hi"}]}`)
	// Client echo: no id (or msg_synth), no status, no phase; content blocks have only type+text
	client := []byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}],"id":"msg_abc"}`)
	cb := canonicalizeBridgeItem(backend)
	cc := canonicalizeBridgeItem(client)
	if !bytes.Equal(cb, cc) {
		t.Fatalf("message with content-block drift must canonicalize equal:\n  backend -> %s\n  client  -> %s", cb, cc)
	}
}

func TestCanonicalizeContentArrayPreservesMultipleBlocks(t *testing.T) {
	backend := []byte(`[{"type":"output_text","annotations":[],"logprobs":[],"text":"a"},{"type":"output_text","annotations":[],"text":"b"}]`)
	want := []byte(`[{"type":"output_text","text":"a"},{"type":"output_text","text":"b"}]`)
	if got := canonicalizeContentArray(backend); !bytes.Equal(got, want) {
		t.Fatalf("content array:\n  got:  %s\n  want: %s", got, want)
	}
}

func TestHTTPToWSBridgeComputeDeltaToleratesContentBlockDrift(t *testing.T) {
	bridge := NewHTTPToWSBridge()
	sessionKey := "session-content-drift"
	req1 := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	// Response output: assistant message with annotations+logprobs in content blocks
	resp1 := []byte(`{"id":"resp-1","output":[{"id":"msg_abc","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":"hello"}]}]}`)
	bridge.CaptureResponse(sessionKey, "resp-1", "gpt-5-codex", "auth-1", req1, resp1)

	// Turn 2: client echoes the assistant message stripped of backend fields
	req2 := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}],"id":"msg_abc"},{"role":"user","content":[{"type":"input_text","text":"thanks"}]}]}`)
	delta, prevRespID := bridge.ComputeDelta(sessionKey, req2, "auth-1")
	if prevRespID != "resp-1" {
		t.Fatalf("content-block drift should not reset bridge; got prevRespID=%q", prevRespID)
	}
	if delta == nil {
		t.Fatal("expected non-nil delta")
	}
	if got := string(delta); got != `[{"role":"user","content":[{"type":"input_text","text":"thanks"}]}]` {
		t.Fatalf("delta = %s", got)
	}
}

func TestHTTPToWSBridgeComputeDeltaToleratesFunctionCallFieldDrift(t *testing.T) {
	// Reproduces the production bug: bridge captures a function_call from
	// backend response.output (with id + status), Droid sends it back in the
	// next turn stripped of those fields. Without canonicalization, byte
	// compare fails and every turn is full_send.
	bridge := NewHTTPToWSBridge()
	sessionKey := "session-fc-drift"
	req1 := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","input":[{"role":"user","content":[{"type":"input_text","text":"run it"}]}]}`)
	// Response output includes the fc_ id + status that Droid will strip.
	resp1 := []byte(`{"id":"resp-1","output":[{"id":"fc_0e7c25c7","type":"function_call","status":"completed","arguments":"{}","call_id":"call_xyz","name":"Execute"}]}`)
	bridge.CaptureResponse(sessionKey, "resp-1", "gpt-5-codex", "auth-1", req1, resp1)

	// Turn 2: Droid echoes the function_call stripped of id+status, in canonical order.
	req2 := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","input":[{"role":"user","content":[{"type":"input_text","text":"run it"}]},{"type":"function_call","call_id":"call_xyz","name":"Execute","arguments":"{}"},{"type":"function_call_output","call_id":"call_xyz","output":"done"}]}`)
	delta, prevRespID := bridge.ComputeDelta(sessionKey, req2, "auth-1")
	if prevRespID != "resp-1" {
		t.Fatalf("function_call field drift should not reset bridge; got prevRespID=%q", prevRespID)
	}
	if got := string(delta); got != `[{"type":"function_call_output","call_id":"call_xyz","output":"done"}]` {
		t.Fatalf("delta = %s", got)
	}
}

func TestNormalizeSynthIDsPreservesContentOccurrences(t *testing.T) {
	// Synth-looking substring inside a text field must NOT be rewritten —
	// only `"id":"msg_synth_N"` key-value pairs are targeted.
	in := []byte(`{"type":"message","id":"real-id","content":"we hit msg_synth_5 bug"}`)
	if got := normalizeSynthIDs(in); !bytes.Equal(got, in) {
		t.Fatalf("content occurrence should not be rewritten; got %s", got)
	}
}

func TestNormalizeSynthIDsFastPathWhenAbsent(t *testing.T) {
	in := []byte(`{"type":"message","id":"msg_01abcdef","content":"hi"}`)
	if got := normalizeSynthIDs(in); !bytes.Equal(got, in) {
		t.Fatalf("payload with no synth IDs should be returned unchanged")
	}
}

func TestHTTPToWSBridgeComputeDeltaToleratesSynthIDDrift(t *testing.T) {
	bridge := NewHTTPToWSBridge()
	sessionKey := "session-synth"
	req1 := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","input":[{"type":"message","id":"msg_synth_0","role":"assistant","content":"hi"}]}`)
	resp1 := []byte(`{"id":"resp-1","output":[{"type":"message","id":"assistant-1"}]}`)
	bridge.CaptureResponse(sessionKey, "resp-1", "gpt-5-codex", "auth-1", req1, resp1)

	// Turn 2: same message at index 0 but Droid's module-global counter has
	// advanced, so the synth ID is now `_5`. The rest of the payload extends
	// the baseline with a new user message.
	req2 := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","input":[{"type":"message","id":"msg_synth_5","role":"assistant","content":"hi"},{"type":"message","id":"assistant-1"},{"type":"message","id":"msg-2"}]}`)
	delta, prevRespID := bridge.ComputeDelta(sessionKey, req2, "auth-1")
	if prevRespID != "resp-1" {
		t.Fatalf("synth ID drift should not reset bridge; got prevRespID=%q", prevRespID)
	}
	if got := string(delta); got != `[{"type":"message","id":"msg-2"}]` {
		t.Fatalf("delta = %s", got)
	}
}

func TestHTTPToWSBridgeComputeDeltaDetectsRealContentDrift(t *testing.T) {
	// Sanity: if the surrounding content actually differs, the prefix check
	// must still reset — normalization is scoped to the id field only.
	bridge := NewHTTPToWSBridge()
	sessionKey := "session-drift"
	req1 := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","input":[{"type":"message","id":"msg_synth_0","role":"assistant","content":"original text"}]}`)
	resp1 := []byte(`{"id":"resp-1","output":[{"type":"message","id":"assistant-1"}]}`)
	bridge.CaptureResponse(sessionKey, "resp-1", "gpt-5-codex", "auth-1", req1, resp1)

	req2 := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","input":[{"type":"message","id":"msg_synth_5","role":"assistant","content":"edited text"},{"type":"message","id":"assistant-1"},{"type":"message","id":"msg-2"}]}`)
	delta, prevRespID := bridge.ComputeDelta(sessionKey, req2, "auth-1")
	if delta != nil || prevRespID != "" {
		t.Fatalf("content change at baseline item should reset bridge; got delta=%s prevRespID=%q", string(delta), prevRespID)
	}
}

func TestHTTPToWSBridgeComputeDeltaUsesInputAndOutputBaseline(t *testing.T) {
	bridge := NewHTTPToWSBridge()
	sessionKey := "session-1"
	requestPayload := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","tools":[{"type":"function","name":"Read"}],"input":[{"type":"message","id":"msg-1"}]}`)
	responsePayload := []byte(`{"id":"resp-1","output":[{"type":"function_call","id":"fc-1","call_id":"call-1"},{"type":"message","id":"assistant-1"}]}`)

	bridge.CaptureResponse(sessionKey, "resp-1", "gpt-5-codex", "auth-1", requestPayload, responsePayload)

	nextPayload := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","tools":[{"type":"function","name":"Read"}],"input":[{"type":"message","id":"msg-1"},{"type":"function_call","id":"fc-1","call_id":"call-1"},{"type":"message","id":"assistant-1"},{"type":"function_call_output","id":"tool-out-1","call_id":"call-1"}]}`)
	delta, prevRespID := bridge.ComputeDelta(sessionKey, nextPayload, "auth-1")
	if prevRespID != "resp-1" {
		t.Fatalf("prevRespID = %q, want resp-1", prevRespID)
	}
	if got := string(delta); got != `[{"type":"function_call_output","id":"tool-out-1","call_id":"call-1"}]` {
		t.Fatalf("delta = %s", got)
	}
}

func TestHTTPToWSBridgeComputeDeltaResetsOnRequestShapeChange(t *testing.T) {
	bridge := NewHTTPToWSBridge()
	sessionKey := "session-shape"
	requestPayload := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","tools":[{"type":"function","name":"Read"}],"input":[{"type":"message","id":"msg-1"}]}`)
	responsePayload := []byte(`{"id":"resp-1","output":[{"type":"message","id":"assistant-1"}]}`)
	bridge.CaptureResponse(sessionKey, "resp-1", "gpt-5-codex", "auth-1", requestPayload, responsePayload)

	changedToolsPayload := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","tools":[{"type":"function","name":"Read"},{"type":"function","name":"Write"}],"input":[{"type":"message","id":"msg-1"},{"type":"message","id":"assistant-1"},{"type":"message","id":"msg-2"}]}`)
	delta, prevRespID := bridge.ComputeDelta(sessionKey, changedToolsPayload, "auth-1")
	if delta != nil || prevRespID != "" {
		t.Fatalf("expected request shape change to reset bridge, got delta=%s prevRespID=%q", string(delta), prevRespID)
	}
}

func TestHTTPToWSBridgeComputeDeltaResetsOnAuthChange(t *testing.T) {
	bridge := NewHTTPToWSBridge()
	sessionKey := "session-auth"
	requestPayload := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","input":[{"type":"message","id":"msg-1"}]}`)
	responsePayload := []byte(`{"id":"resp-1","output":[{"type":"message","id":"assistant-1"}]}`)
	bridge.CaptureResponse(sessionKey, "resp-1", "gpt-5-codex", "auth-1", requestPayload, responsePayload)

	nextPayload := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","input":[{"type":"message","id":"msg-1"},{"type":"message","id":"assistant-1"},{"type":"message","id":"msg-2"}]}`)
	delta, prevRespID := bridge.ComputeDelta(sessionKey, nextPayload, "auth-2")
	if delta != nil || prevRespID != "" {
		t.Fatalf("expected auth change to reset bridge, got delta=%s prevRespID=%q", string(delta), prevRespID)
	}
}

// TestHTTPToWSBridgeResetForcesFullPayloadOnNextDelta verifies Reset clears bridge state before retry.
func TestHTTPToWSBridgeResetForcesFullPayloadOnNextDelta(t *testing.T) {
	bridge := NewHTTPToWSBridge()
	sessionKey := "session-retry-after-reconnect"
	req1 := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","input":[{"role":"user","content":[{"type":"input_text","text":"first message"}]}]}`)
	resp1 := []byte(`{"id":"resp-1","output":[{"type":"message","id":"assistant-1","role":"assistant","content":[{"type":"output_text","text":"reply"}]}]}`)
	bridge.CaptureResponse(sessionKey, "resp-1", "gpt-5-codex", "auth-1", req1, resp1)

	// Sanity: a normal turn-2 produces a delta + previous_response_id.
	req2 := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","input":[{"role":"user","content":[{"type":"input_text","text":"first message"}]},{"type":"message","id":"assistant-1","role":"assistant","content":[{"type":"output_text","text":"reply"}]},{"role":"user","content":[{"type":"input_text","text":"second message"}]}]}`)
	delta, prevRespID := bridge.ComputeDelta(sessionKey, req2, "auth-1")
	if delta == nil || prevRespID != "resp-1" {
		t.Fatalf("baseline turn-2 should produce delta with prevRespID=resp-1, got delta=%v prevRespID=%q", delta != nil, prevRespID)
	}

	// Simulate the reconnect-during-delta path: the production code calls
	// bridge.Reset(sessionKey) BEFORE the retry's doBridgedStream so that the
	// retry's ComputeDelta returns nil → full payload sent on the new conn.
	bridge.Reset(sessionKey)

	// After reset, ComputeDelta MUST return nil so doBridgedStream falls back
	// to sending the full payload. Returning a delta here would re-trip the
	// "session forgetting" symptom: the new WS connection has no record of
	// resp-1 in its session-level state, so a delta keyed on it would either
	// be rejected upstream or silently drop the user's most recent message.
	delta2, prevRespID2 := bridge.ComputeDelta(sessionKey, req2, "auth-1")
	if delta2 != nil || prevRespID2 != "" {
		t.Fatalf("after Reset, ComputeDelta must return nil delta and empty prevRespID to force full_send, got delta=%s prevRespID=%q", string(delta2), prevRespID2)
	}
}

// TestPatchResponseOutputIfMissing exercises both ordering modes (indexed
// + fallback) and the "already populated" early-return. Shared helper used
// by both HTTP Codex execution and bridged WS capture; divergence here
// would silently produce two different baselines for the same upstream
// response.
func TestPatchResponseOutputIfMissing(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		byIndex  map[int64][]byte
		fallback [][]byte
		want     string
		patched  bool
	}{
		{
			name:    "no items collected → no patch",
			payload: `{"response":{}}`,
			want:    `{"response":{}}`,
			patched: false,
		},
		{
			name:    "payload already has items → no patch",
			payload: `{"response":{"output":[{"type":"message"}]}}`,
			byIndex: map[int64][]byte{0: []byte(`{"type":"function_call"}`)},
			want:    `{"response":{"output":[{"type":"message"}]}}`,
			patched: false,
		},
		{
			name:    "indexed items sorted ascending by index",
			payload: `{"response":{}}`,
			byIndex: map[int64][]byte{
				2: []byte(`{"type":"third"}`),
				0: []byte(`{"type":"first"}`),
				1: []byte(`{"type":"second"}`),
			},
			want:    `{"response":{"output":[{"type":"first"},{"type":"second"},{"type":"third"}]}}`,
			patched: true,
		},
		{
			name:    "fallback items appended after indexed ones",
			payload: `{"response":{}}`,
			byIndex: map[int64][]byte{
				0: []byte(`{"type":"indexed_zero"}`),
			},
			fallback: [][]byte{
				[]byte(`{"type":"fb_one"}`),
				[]byte(`{"type":"fb_two"}`),
			},
			want:    `{"response":{"output":[{"type":"indexed_zero"},{"type":"fb_one"},{"type":"fb_two"}]}}`,
			patched: true,
		},
		{
			name:     "fallback only — preserved in arrival order",
			payload:  `{"response":{}}`,
			fallback: [][]byte{[]byte(`{"type":"a"}`), []byte(`{"type":"b"}`)},
			want:     `{"response":{"output":[{"type":"a"},{"type":"b"}]}}`,
			patched:  true,
		},
		{
			name:    "empty response.output array → still patched",
			payload: `{"response":{"output":[]}}`,
			byIndex: map[int64][]byte{0: []byte(`{"type":"item"}`)},
			want:    `{"response":{"output":[{"type":"item"}]}}`,
			patched: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, patched := patchResponseOutputIfMissing([]byte(tc.payload), tc.byIndex, tc.fallback)
			if patched != tc.patched {
				t.Errorf("patched = %v, want %v", patched, tc.patched)
			}
			if string(got) != tc.want {
				t.Errorf("got %s, want %s", string(got), tc.want)
			}
		})
	}
}

// TestCollectOutputItem confirms classification by output_index vs fallback.
func TestCollectOutputItem(t *testing.T) {
	byIndex := map[int64][]byte{}
	var fallback [][]byte

	fallback = collectOutputItem([]byte(`{"item":{"type":"a"},"output_index":0}`), byIndex, fallback)
	fallback = collectOutputItem([]byte(`{"item":{"type":"b"}}`), byIndex, fallback) // no index → fallback
	fallback = collectOutputItem([]byte(`{"item":{"type":"c"},"output_index":2}`), byIndex, fallback)
	fallback = collectOutputItem([]byte(`{"output_index":99}`), byIndex, fallback) // missing item → ignored

	if len(byIndex) != 2 {
		t.Errorf("byIndex len = %d, want 2", len(byIndex))
	}
	if string(byIndex[0]) != `{"type":"a"}` || string(byIndex[2]) != `{"type":"c"}` {
		t.Errorf("byIndex contents wrong: %v", byIndex)
	}
	if len(fallback) != 1 || string(fallback[0]) != `{"type":"b"}` {
		t.Errorf("fallback wrong: %v", fallback)
	}
}

// TestCanonicalizeContentBlockTable covers every content-block type in
// contentBlockFieldsCanonical: confirms backend-only fields (annotations,
// logprobs) are stripped, declared fields are preserved, and most
// importantly that input_file's distinct files don't collapse to the same
// canonical form (file_data and filename now act as discriminators).
func TestCanonicalizeContentBlockTable(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "output_text strips annotations and logprobs",
			in:   `{"type":"output_text","annotations":[],"logprobs":[],"text":"hello"}`,
			want: `{"type":"output_text","text":"hello"}`,
		},
		{
			name: "output_text without extra fields passes through",
			in:   `{"type":"output_text","text":"hello"}`,
			want: `{"type":"output_text","text":"hello"}`,
		},
		{
			name: "refusal preserves refusal text",
			in:   `{"type":"refusal","refusal":"sorry"}`,
			want: `{"type":"refusal","refusal":"sorry"}`,
		},
		{
			name: "input_text passes through",
			in:   `{"type":"input_text","text":"hi"}`,
			want: `{"type":"input_text","text":"hi"}`,
		},
		{
			name: "input_image preserves image_url and detail",
			in:   `{"type":"input_image","image_url":"https://example.com/x.png","detail":"high"}`,
			want: `{"type":"input_image","image_url":"https://example.com/x.png","detail":"high"}`,
		},
		{
			name: "input_file with file_id only",
			in:   `{"type":"input_file","file_id":"file-abc"}`,
			want: `{"type":"input_file","file_id":"file-abc"}`,
		},
		{
			name: "input_file with file_data and filename — both retained as discriminators",
			in:   `{"type":"input_file","file_data":"YmFzZTY0LWFiYw==","filename":"a.txt"}`,
			want: `{"type":"input_file","file_data":"YmFzZTY0LWFiYw==","filename":"a.txt"}`,
		},
		{
			name: "input_file with all three fields",
			in:   `{"type":"input_file","file_id":"file-abc","file_data":"YmFzZTY0","filename":"a.txt"}`,
			want: `{"type":"input_file","file_id":"file-abc","file_data":"YmFzZTY0","filename":"a.txt"}`,
		},
		{
			name: "unknown type passes through untouched",
			in:   `{"type":"future_block","foo":"bar","baz":[1,2]}`,
			want: `{"type":"future_block","foo":"bar","baz":[1,2]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := canonicalizeContentBlock([]byte(tc.in))
			if !bytes.Equal(got, []byte(tc.want)) {
				t.Errorf("canonicalizeContentBlock got %s, want %s", string(got), tc.want)
			}
		})
	}
}

// TestCanonicalizeContentBlockInputFileDiscrimination ensures distinct input_file blocks stay distinct.
func TestCanonicalizeContentBlockInputFileDiscrimination(t *testing.T) {
	fileA := canonicalizeContentBlock([]byte(`{"type":"input_file","file_data":"AAAA","filename":"a.txt"}`))
	fileB := canonicalizeContentBlock([]byte(`{"type":"input_file","file_data":"BBBB","filename":"b.txt"}`))
	if bytes.Equal(fileA, fileB) {
		t.Fatalf("distinct input_file blocks must not canonicalize identical:\n  fileA=%s\n  fileB=%s", string(fileA), string(fileB))
	}
}

// TestHTTPToWSBridgeComputeDeltaConcurrentSessions verifies concurrent sessions do not block each other.
func TestHTTPToWSBridgeComputeDeltaConcurrentSessions(t *testing.T) {
	bridge := NewHTTPToWSBridge()

	// Seed two sessions with valid baselines so neither call hits the
	// fast-path early-return; both exercise the full validation/canonicalization
	// path.
	for _, sk := range []string{"sess-A", "sess-B"} {
		req := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
		resp := []byte(`{"id":"resp-1","output":[{"type":"message","id":"asst-1","role":"assistant","content":[{"type":"output_text","text":"reply"}]}]}`)
		bridge.CaptureResponse(sk, "resp-1", "gpt-5-codex", "auth-1", req, resp)
	}

	follow := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"message","id":"asst-1","role":"assistant","content":[{"type":"output_text","text":"reply"}]},{"role":"user","content":[{"type":"input_text","text":"more"}]}]}`)

	// Hammer both sessions concurrently; under -race this catches data races
	// on the snapshot/validate split.
	const iterations = 50
	done := make(chan struct{}, 2)
	for _, sk := range []string{"sess-A", "sess-B"} {
		sk := sk
		go func() {
			for i := 0; i < iterations; i++ {
				delta, prev := bridge.ComputeDelta(sk, follow, "auth-1")
				if delta == nil || prev != "resp-1" {
					t.Errorf("session %s iteration %d: expected delta+prevRespID, got delta=%v prev=%q", sk, i, delta != nil, prev)
					done <- struct{}{}
					return
				}
			}
			done <- struct{}{}
		}()
	}
	<-done
	<-done
}

// TestHTTPToWSBridgeBaselineCountAfterResetIsZero verifies the baseline
// counter is also reset, so telemetry's baselineBefore reads 0 after a
// retry-triggered Reset (matches the "fresh session" semantics the retry
// expects).
func TestHTTPToWSBridgeBaselineCountAfterResetIsZero(t *testing.T) {
	bridge := NewHTTPToWSBridge()
	sessionKey := "session-baseline-reset"
	req1 := []byte(`{"model":"gpt-5-codex","instructions":"be helpful","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	resp1 := []byte(`{"id":"resp-1","output":[{"type":"message","id":"assistant-1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`)
	bridge.CaptureResponse(sessionKey, "resp-1", "gpt-5-codex", "auth-1", req1, resp1)

	if got := bridge.BaselineCount(sessionKey); got == 0 {
		t.Fatalf("expected non-zero baseline after capture, got %d", got)
	}

	bridge.Reset(sessionKey)

	if got := bridge.BaselineCount(sessionKey); got != 0 {
		t.Fatalf("expected baseline=0 after Reset, got %d", got)
	}
}
