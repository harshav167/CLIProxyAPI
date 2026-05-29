// Package executor — remote server-side compaction for Codex Responses API.
//
// Cursor (and other BYOK clients) don't honor our reported context_window
// and send requests that exceed upstream's 272K token ceiling. The bridge
// path gets a 200 OK with an empty stream, and 500s bubble up to the client.
//
// OpenAI's Codex backend exposes a purpose-built /v1/responses/compact
// endpoint that Codex CLI uses for its own auto-compaction. The endpoint
// accepts a Responses-API payload, runs server-side summarization, and
// returns an encrypted compaction_summary item which the CALLER splices
// back into the next /v1/responses request's input[] array. The bridge
// already speaks Responses API — so the splice is natural.
//
// We detect oversized payloads before forwarding to the upstream WS,
// call /responses/compact to turn the middle of the conversation into a
// single opaque compaction item, then forward the shrunken payload.
package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// remoteCompactThresholdBytes is how big a translated Responses-API payload
// needs to be before we trigger remote compaction. 700KB ≈ 175K tokens at the
// pessimistic 4-bytes-per-token estimate, well below upstream's 272K ceiling
// but with enough headroom to account for the roughly-20% bloat translated
// payloads incur vs. the client's original request.
const remoteCompactThresholdBytes = 700_000

// remoteCompactMinKeepTurns is how many recent messages we preserve full-
// resolution on either side of the compaction summary. Keeping a healthy
// tail prevents the model from losing short-term context (e.g. "open the
// file I just read" references).
const remoteCompactMinKeepTurns = 20

// remoteCompactFailureCooldown is how long a per-(session,auth) compact
// failure suppresses subsequent compaction attempts. When the upstream
// /responses/compact endpoint is degraded or returning errors, we don't
// want every oversized turn to pay the same network round-trip + timeout
// before falling back to forwarding the original payload — that piles
// the failure cost on top of the original failure mode. After this
// window, we try again once. If it still fails, the cooldown re-arms.
const remoteCompactFailureCooldown = 30 * time.Second

// compactCooldownStore tracks the most recent compact failure timestamp
// per "session|authID" key. Lookups are lock-free reads; writes are rare
// (only on failure) so a single mutex is fine.
//
// Eviction strategy: lazy on lookup (compactInCooldown removes a key when
// it observes the entry is past the cooldown window) PLUS a periodic
// background pruner (compactCooldownPrune) that walks the map and removes
// expired entries even if no lookup ever revisits them. Without the
// background pass, one-off failed sessions that never retry would leave
// entries sitting in the map indefinitely, growing memory unboundedly in
// long-lived processes.
var (
	compactCooldownMu         sync.Mutex
	compactCooldown           = map[string]time.Time{}
	compactCooldownPrunerOnce sync.Once
)

// compactCooldownPruneInterval is how often the background pruner walks the
// cooldown map. Set conservatively (well above remoteCompactFailureCooldown)
// so pruning is genuinely periodic background work, not contention with hot
// lookups.
const compactCooldownPruneInterval = 5 * time.Minute

// startCompactCooldownPruner kicks off the background pruner exactly once
// per process. Safe to call from multiple init paths — sync.Once gates it.
//
// The pruner is intentionally lightweight: a 5-minute ticker that grabs the
// mutex briefly, walks the map, and deletes expired entries. For typical
// proxy load (hundreds of sessions, low compact failure rate) the map stays
// well under 1K entries between prunes.
func startCompactCooldownPruner() {
	compactCooldownPrunerOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(compactCooldownPruneInterval)
			defer ticker.Stop()
			for range ticker.C {
				compactCooldownPrune()
			}
		}()
	})
}

// compactCooldownPrune evicts expired entries from the cooldown map.
// Exposed (lowercase, package-internal) so tests can drive it directly
// without waiting on the ticker.
func compactCooldownPrune() {
	now := time.Now()
	compactCooldownMu.Lock()
	defer compactCooldownMu.Unlock()
	for k, when := range compactCooldown {
		if now.Sub(when) >= remoteCompactFailureCooldown {
			delete(compactCooldown, k)
		}
	}
}

// compactInCooldown reports whether the (session, auth) pair is within the
// cooldown window from a recent failure. Returns false (and lazily evicts
// the entry) when the cooldown has elapsed.
func compactInCooldown(sessionKey, authID string) bool {
	if sessionKey == "" {
		return false
	}
	key := sessionKey + "|" + authID
	compactCooldownMu.Lock()
	defer compactCooldownMu.Unlock()
	when, ok := compactCooldown[key]
	if !ok {
		return false
	}
	if time.Since(when) >= remoteCompactFailureCooldown {
		delete(compactCooldown, key)
		return false
	}
	return true
}

// markCompactFailure stamps the (session, auth) pair so subsequent
// maybeRemoteCompact calls within remoteCompactFailureCooldown skip the
// /responses/compact attempt. Also lazily kicks off the background pruner
// (sync.Once-gated, so first call wins; subsequent calls are no-ops). This
// keeps the pruner off when no compaction failures have ever occurred and
// auto-starts it the moment the cooldown map gains its first entry.
func markCompactFailure(sessionKey, authID string) {
	if sessionKey == "" {
		return
	}
	startCompactCooldownPruner()
	key := sessionKey + "|" + authID
	compactCooldownMu.Lock()
	compactCooldown[key] = time.Now()
	compactCooldownMu.Unlock()
}

// maybeRemoteCompact examines a Responses-API payload and, when it exceeds
// remoteCompactThresholdBytes, asks /v1/responses/compact to summarize the
// middle and splices the returned compaction_summary item into input[].
//
// Returns the (possibly rewritten) payload + a boolean indicating whether
// compaction was applied. Errors are logged + swallowed: on failure the
// original payload flows through unchanged (so the user gets the normal
// 500 instead of a double-failure).
//
// This operates on the outbound Responses-API payload (NOT the inbound
// chat-completions body). Call sites: doBridgedStream, right before
// bridge.ComputeDelta, so compaction runs before delta math.
//
// cfg is threaded so the /responses/compact request honors the same proxy
// configuration as the main codex paths (via helps.NewProxyAwareHTTPClient).
// Without it, compaction requests would fail behind corporate HTTP proxies
// and the oversized original payload would still reach upstream.
func maybeRemoteCompact(
	ctx context.Context,
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	payload []byte,
	sessionKey string,
) (out []byte, compacted bool) {
	// Disabled by default. Cursor's own client-side compaction handles
	// oversized contexts effectively when the model registry advertises the
	// correct context window (e.g. 272K for gpt-5.5). Enable via
	// `codex-remote-compaction.enabled: true` in config.yaml only if you
	// have a chat-completions client that does NOT do its own compaction
	// and is hitting upstream context limits despite normal bridge flow.
	//
	// Defensive: when cfg is nil (test contexts, unit tests on the helper),
	// treat as disabled. Production code paths always pass a real cfg.
	if cfg == nil || !cfg.CodexRemoteCompaction.Enabled {
		return payload, false
	}
	// Cursor has native compaction and must keep owning that lifecycle. The
	// proxy's remote /responses/compact splice can leave Cursor waiting on a
	// turn shape it did not initiate, so never apply it to Cursor traffic even
	// when the experimental remote-compaction flag is enabled.
	if IsCursorClient(ctx) {
		return payload, false
	}
	// Skip when session key is empty. The (session, auth) cooldown that
	// guards against degraded-endpoint thrashing is keyed by sessionKey;
	// with an empty key the cooldown can't protect us, so every oversized
	// turn during a degraded compact endpoint would pay the full network
	// round-trip + timeout repeatedly. Better to skip compaction for
	// stateless / session-less requests than to risk uncooled-down
	// thrashing. The threshold-vs-no-compaction trade-off is worth it
	// because clients that don't supply a stable session key can't reliably
	// chain multi-turn state anyway.
	if sessionKey == "" {
		return payload, false
	}
	if len(payload) < remoteCompactThresholdBytes {
		return payload, false
	}
	// Skip if a recent compact attempt for this (session, auth) failed.
	// Without this gate, every oversized turn during a degraded compact
	// endpoint pays the full network round-trip + timeout BEFORE falling
	// back to forwarding the oversized payload — multiplying the latency
	// of the worst path. The cooldown re-arms on each failure.
	if compactInCooldown(sessionKey, authIDForBridge(auth)) {
		return payload, false
	}
	input := gjson.GetBytes(payload, "input")
	if !input.IsArray() {
		return payload, false
	}
	items := input.Array()
	if len(items) <= remoteCompactMinKeepTurns+1 {
		// Too few messages to safely compact — head/tail would overlap.
		return payload, false
	}

	// Partition: keep the first message (usually a user turn anchoring the
	// conversation) + the last remoteCompactMinKeepTurns messages. Everything
	// in between gets summarized.
	keepHead := 1
	keepTail := remoteCompactMinKeepTurns
	if len(items) <= keepHead+keepTail {
		return payload, false
	}
	// Expand keepTail UP THE CONVERSATION until the boundary doesn't split
	// a function_call / function_call_output pair. Responses-API rejects
	// the rewritten input if a function_call_output is retained without its
	// matching function_call (or vice versa). Also adjust so we never leave
	// a reasoning item disassociated from its following message. Reasoning
	// items carry state for the assistant's next turn and must travel with
	// the immediately-following (assistant or tool) item.
	keepTail = adjustKeepTailForPairs(items, keepHead, keepTail)
	if len(items) <= keepHead+keepTail {
		return payload, false
	}
	middle := items[keepHead : len(items)-keepTail]

	// Build the compact request: same model/instructions/tools as the main
	// request, but only the slice we want summarized as input.
	compactReq := []byte(`{}`)
	compactReq, _ = sjson.SetBytes(compactReq, "model", gjson.GetBytes(payload, "model").String())
	compactReq, _ = sjson.SetBytes(compactReq, "instructions", gjson.GetBytes(payload, "instructions").String())
	if tools := gjson.GetBytes(payload, "tools"); tools.Exists() {
		compactReq, _ = sjson.SetRawBytes(compactReq, "tools", []byte(tools.Raw))
	} else {
		compactReq, _ = sjson.SetRawBytes(compactReq, "tools", []byte("[]"))
	}
	compactReq, _ = sjson.SetBytes(compactReq, "parallel_tool_calls", false)
	if reasoning := gjson.GetBytes(payload, "reasoning"); reasoning.Exists() {
		compactReq, _ = sjson.SetRawBytes(compactReq, "reasoning", []byte(reasoning.Raw))
	}

	// Serialize middle as the compact input array. Pre-size buf from the
	// known total Raw length (sum of item.Raw + commas + brackets) to avoid
	// grow-from-empty append amplification on this large-payload path —
	// middle frequently contains hundreds of items totaling >500KB.
	middleSize := 2 // brackets
	for i, item := range middle {
		if i > 0 {
			middleSize++ // comma
		}
		middleSize += len(item.Raw)
	}
	buf := make([]byte, 0, middleSize)
	buf = append(buf, '[')
	for i, item := range middle {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, item.Raw...)
	}
	buf = append(buf, ']')
	compactReq, _ = sjson.SetRawBytes(compactReq, "input", buf)

	log.Infof("codex remote-compact: triggering session=%s middle_items=%d head_items=%d tail_items=%d body_bytes=%d",
		sessionKey, len(middle), keepHead, keepTail, len(payload))

	summaryItem, err := postResponsesCompact(ctx, cfg, auth, compactReq)
	if err != nil {
		log.Warnf("codex remote-compact: /responses/compact failed session=%s err=%v — forwarding original payload (suppressing further compact attempts for %s)", sessionKey, err, remoteCompactFailureCooldown)
		markCompactFailure(sessionKey, authIDForBridge(auth))
		return payload, false
	}
	if len(summaryItem) == 0 {
		log.Warnf("codex remote-compact: /responses/compact returned no compaction_summary session=%s — forwarding original (suppressing further compact attempts for %s)", sessionKey, remoteCompactFailureCooldown)
		markCompactFailure(sessionKey, authIDForBridge(auth))
		return payload, false
	}

	// Splice: [head] + [summary_item] + [tail]. Pre-size from known Raw
	// lengths (head items + summary item + tail items + commas + brackets)
	// to avoid append amplification on the splice path.
	spliceSize := 2 + len(summaryItem) // brackets + summary
	for i := 0; i < keepHead; i++ {
		spliceSize += len(items[i].Raw) + 1 // item + leading comma
	}
	for i := len(items) - keepTail; i < len(items); i++ {
		spliceSize += len(items[i].Raw) + 1 // item + leading comma
	}
	newInput := make([]byte, 0, spliceSize)
	newInput = append(newInput, '[')
	for i := 0; i < keepHead; i++ {
		if i > 0 {
			newInput = append(newInput, ',')
		}
		newInput = append(newInput, items[i].Raw...)
	}
	if keepHead > 0 {
		newInput = append(newInput, ',')
	}
	newInput = append(newInput, summaryItem...)
	for i := len(items) - keepTail; i < len(items); i++ {
		newInput = append(newInput, ',')
		newInput = append(newInput, items[i].Raw...)
	}
	newInput = append(newInput, ']')

	out, err = sjson.SetRawBytes(payload, "input", newInput)
	if err != nil {
		log.Warnf("codex remote-compact: splice failed session=%s err=%v — forwarding original", sessionKey, err)
		return payload, false
	}
	log.Infof("codex remote-compact: applied session=%s old_bytes=%d new_bytes=%d saved=%d",
		sessionKey, len(payload), len(out), len(payload)-len(out))
	return out, true
}

// postResponsesCompact calls the upstream /v1/responses/compact endpoint with
// the supplied Codex-auth and compaction payload. Returns the raw JSON of the
// compaction_summary item if the server produced one.
//
// Wire mechanics (URL, auth headers, proxy-aware HTTP client, status handling)
// are delegated to doCodexCompactRequest so this function and
// CodexExecutor.executeCompact stay in lockstep on transport behavior. They
// diverge only in HOW they prepare the request body (executeCompact runs
// the full translator chain for client-initiated compaction; this function
// works with an already-prepared codex-format body assembled in
// maybeRemoteCompact).
func postResponsesCompact(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, body []byte) ([]byte, error) {
	data, err := doCodexCompactRequest(ctx, cfg, auth, body)
	if err != nil {
		return nil, err
	}

	// The response has output: [...user msgs..., { type:"compaction_summary", id:"cmp_...", encrypted_content:"..." }].
	// We only need the compaction_summary item; splice it into the caller's input[].
	output := gjson.GetBytes(data, "output")
	if !output.IsArray() {
		return nil, fmt.Errorf("unexpected response shape: no output array")
	}
	var summary gjson.Result
	output.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "compaction_summary" {
			summary = item
			return false
		}
		return true
	})
	if !summary.Exists() {
		return nil, fmt.Errorf("no compaction_summary item in response")
	}
	return []byte(summary.Raw), nil
}

// doCodexCompactRequest performs the HTTP POST to /v1/responses/compact
// using the same proxy-aware client, Codex auth headers, recording hooks,
// timeout policy, and status-error type as the CodexExecutor.executeCompact
// path. Centralized so the two compaction call sites — client-initiated
// (executeCompact, prepares body via the full translator chain) and
// bridge-initiated (postResponsesCompact, body already in codex format) —
// can never diverge on transport behavior.
//
// Timeout: 0 (no client-side cap) — matches executeCompact and the project
// guidance that post-credential network operations rely on the context
// deadline rather than an explicit timeout. The earlier 60s cap was an
// inadvertent divergence.
//
// Recording: emits the same helps.RecordAPIRequest / RecordAPIResponseError /
// RecordAPIResponseMetadata / AppendAPIResponseChunk telemetry that the
// executeCompact path emits, so observability is identical regardless of
// which call site initiated the compact.
//
// Status errors: returns newCodexStatusErr (not fmt.Errorf) so callers can
// pattern-match status codes for retry logic the same way they do for
// executeCompact responses.
func doCodexCompactRequest(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, body []byte) ([]byte, error) {
	apiKey, baseURL := codexCreds(auth)
	if apiKey == "" {
		return nil, fmt.Errorf("no codex api_key for auth %s", authIDForBridge(auth))
	}
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}
	url := strings.TrimSuffix(baseURL, "/") + "/responses/compact"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyCodexHeaders(req, auth, apiKey, false, cfg)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// Bridge-side caller doesn't need response headers (only the body for
	// splicing the compaction_summary into input[]).
	data, _, err := doCodexCompactTransport(ctx, cfg, auth, req, body, "codex")
	return data, err
}

// doCodexCompactTransport is the shared transport core for /v1/responses/compact:
// emits the standard helps.RecordAPI* telemetry, uses a proxy-aware client with
// the project's standard timeout policy (0 = rely on ctx), reads the body,
// status-checks with newCodexStatusErr.
//
// Both compaction call sites use it:
//   - doCodexCompactRequest (bridge-initiated): builds the request via raw
//     http.NewRequestWithContext + applyCodexHeaders, then calls this.
//   - CodexExecutor.executeCompact (client-initiated): builds the request via
//     e.cacheHelper (which adds Session_id header + injects prompt_cache_key
//     for Claude/openai-compat clients) + applyCodexHeaders, then calls this.
//
// The body parameter is captured for the RecordAPIRequest log; req should
// already have the body set as its Body. provider is "codex" for both
// callers but parameterized so future executors (claude-compact, etc.) can
// reuse the same transport with their own provider tag.
//
// Consolidating here means timeout policy, recording hooks, and status-error
// type can never drift between paths.
func doCodexCompactTransport(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, req *http.Request, body []byte, provider string) ([]byte, http.Header, error) {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, cfg, helps.UpstreamRequestLog{
		URL:       req.URL.String(),
		Method:    req.Method,
		Headers:   req.Header.Clone(),
		Body:      body,
		Provider:  provider,
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	// Project standard: timeout=0 (rely on ctx), proxy-aware client.
	client := helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 0)
	resp, err := client.Do(req)
	if err != nil {
		helps.RecordAPIResponseError(ctx, cfg, err)
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respHeaders := resp.Header.Clone()
	helps.RecordAPIResponseMetadata(ctx, cfg, resp.StatusCode, respHeaders)
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, cfg, err)
		return nil, respHeaders, err
	}
	helps.AppendAPIResponseChunk(ctx, cfg, data)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, respHeaders, newCodexStatusErr(resp.StatusCode, data)
	}
	return data, respHeaders, nil
}

// adjustKeepTailForPairs grows keepTail so the compaction boundary never
// splits a semantic pair. Expands the retained suffix UP the conversation
// (toward the head) while any of the following are true at the boundary:
//
//  1. The first retained item is a function_call_output without its matching
//     function_call in the retained tail. The matching function_call lives
//     somewhere in the compacted middle — walking up until we include it
//     (matched by call_id) keeps the pair intact.
//
//  2. The last compacted item is a function_call whose function_call_output
//     is in the retained tail. Same remedy — grow keepTail to include the
//     function_call so the pair is evicted together into the summary.
//
//  3. The first retained item is a plain message preceded by a reasoning
//     item in the compacted middle. Reasoning items carry next-turn state
//     and are expected to travel with the message they precede.
//
// Returns the adjusted keepTail. Guarantees the returned value doesn't
// exceed len(items)-keepHead (i.e. never grows past what's compactable).
func adjustKeepTailForPairs(items []gjson.Result, keepHead, keepTail int) int {
	maxTail := len(items) - keepHead
	if keepTail >= maxTail {
		return maxTail
	}
	// Cap how many extra items we'll absorb so pathological inputs don't
	// turn compaction into a no-op. 30 is generous — a tool-call run of
	// that depth is rare and still leaves plenty of middle to summarize.
	const maxExpand = 30
	for grown := 0; grown < maxExpand && keepTail < maxTail; grown++ {
		boundary := len(items) - keepTail
		firstRetained := items[boundary]
		lastCompacted := items[boundary-1]

		firstType := firstRetained.Get("type").String()
		lastType := lastCompacted.Get("type").String()

		// Case 1: retained function_call_output needs its matching
		// function_call. Grow tail until the function_call (by call_id)
		// is included — that way the pair evicts together.
		if firstType == "function_call_output" {
			callID := firstRetained.Get("call_id").String()
			if callID != "" && !tailContainsMatchingFunctionCall(items, boundary, callID) {
				keepTail++
				continue
			}
		}
		// Case 2: compacted-side function_call whose output is retained.
		// Grow tail so the function_call is also retained (pair stays
		// in the tail, no orphan compaction).
		if lastType == "function_call" {
			callID := lastCompacted.Get("call_id").String()
			if callID != "" && tailContainsMatchingFunctionCallOutput(items, boundary, callID) {
				keepTail++
				continue
			}
		}
		// Case 3: compacted-side reasoning item immediately before the
		// retained boundary. Grow tail so the message + reasoning travel
		// together.
		if lastType == "reasoning" {
			keepTail++
			continue
		}
		break
	}
	if keepTail > maxTail {
		keepTail = maxTail
	}
	return keepTail
}

// tailContainsMatchingFunctionCall reports whether the items slice at
// indices [boundary, len(items)) contains a function_call with the given
// call_id. Used by adjustKeepTailForPairs case 1.
func tailContainsMatchingFunctionCall(items []gjson.Result, boundary int, callID string) bool {
	for i := boundary; i < len(items); i++ {
		if items[i].Get("type").String() == "function_call" && items[i].Get("call_id").String() == callID {
			return true
		}
	}
	return false
}

// tailContainsMatchingFunctionCallOutput reports whether the items slice at
// indices [boundary, len(items)) contains a function_call_output with the
// given call_id. Used by adjustKeepTailForPairs case 2.
func tailContainsMatchingFunctionCallOutput(items []gjson.Result, boundary int, callID string) bool {
	for i := boundary; i < len(items); i++ {
		if items[i].Get("type").String() == "function_call_output" && items[i].Get("call_id").String() == callID {
			return true
		}
	}
	return false
}
