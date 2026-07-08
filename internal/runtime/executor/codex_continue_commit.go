package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

// Default Codex client context window when the model registry has no entry.
// Matches internal/registry/models/codex_client_models.json context_window.
const codexContinueDefaultContextWindow = 272000

type codexOutputCommitPolicy string

const (
	codexCommitLiveBarrier     codexOutputCommitPolicy = "live_commit_barrier"
	codexCommitOnCleanTerminal codexOutputCommitPolicy = "on_clean_terminal"
)

type codexFoldIdentity struct {
	visibleResponseID          string
	upstreamPreviousResponseID string
	hiddenResponseIDs          []string
}

type codexFoldRoundInfo struct {
	Number          int
	ResponseID      string
	ReasoningTokens int
	Usage           codexUsage
	TerminalType    string
	StoppedReason   string
}

type codexUsage struct {
	InputTokens     int64 `json:"input_tokens,omitempty"`
	CachedTokens    int64 `json:"cached_tokens,omitempty"`
	OutputTokens    int64 `json:"output_tokens,omitempty"`
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	TotalTokens     int64 `json:"total_tokens,omitempty"`
}

type codexOutputItem struct {
	OutputIndex int64
	Item        []byte
}

type codexFoldState struct {
	originalInput    []any
	replayTail       []any
	rounds           []codexFoldRoundInfo
	billedUsage      codexUsage
	firstUsage       codexUsage
	committedOutput  []codexOutputItem
	responseIdentity codexFoldIdentity
	nextSequence     int64
	nextOutputIndex  int64
	openedAttempts   int
	zeroStalls       int
}

func newCodexFoldState(baseBody []byte) *codexFoldState {
	state := &codexFoldState{nextSequence: -1}
	input := gjson.GetBytes(baseBody, "input")
	if input.Exists() && input.IsArray() {
		_ = json.Unmarshal([]byte(input.Raw), &state.originalInput)
	}
	return state
}

func (s *codexFoldState) noteForwardedPayload(payload []byte) {
	if s == nil || len(payload) == 0 {
		return
	}
	eventType := gjson.GetBytes(payload, "type").String()
	if eventType == "response.created" && s.responseIdentity.visibleResponseID == "" {
		s.responseIdentity.visibleResponseID = gjson.GetBytes(payload, "response.id").String()
	}
	if seq := gjson.GetBytes(payload, "sequence_number"); seq.Exists() && seq.Int() > s.nextSequence {
		s.nextSequence = seq.Int()
	}
	if outputIndex := gjson.GetBytes(payload, "output_index"); outputIndex.Exists() && outputIndex.Int() >= s.nextOutputIndex {
		s.nextOutputIndex = outputIndex.Int() + 1
	}
}

func (s *codexFoldState) addRound(roundNo int, roundOut codexContinueFoldOutput, tokens int) {
	if s == nil {
		return
	}
	usage := codexUsageFromMap(roundOut.usage)
	if len(s.rounds) == 0 {
		s.firstUsage = usage
	} else if roundOut.responseID != "" {
		s.responseIdentity.hiddenResponseIDs = append(s.responseIdentity.hiddenResponseIDs, roundOut.responseID)
	}
	s.billedUsage.add(usage)
	s.responseIdentity.upstreamPreviousResponseID = roundOut.responseID
	s.rounds = append(s.rounds, codexFoldRoundInfo{
		Number:          roundNo,
		ResponseID:      roundOut.responseID,
		ReasoningTokens: tokens,
		Usage:           usage,
		TerminalType:    roundOut.terminalType,
	})
}

func (s *codexFoldState) appendReplay(reasoningItems []map[string]any, marker map[string]any) {
	if s == nil {
		return
	}
	for _, item := range reasoningItems {
		s.replayTail = append(s.replayTail, item)
		payload, err := json.Marshal(item)
		if err != nil {
			continue
		}
		s.committedOutput = append(s.committedOutput, codexOutputItem{OutputIndex: s.nextOutputIndex, Item: payload})
		s.nextOutputIndex++
	}
	if marker != nil {
		s.replayTail = append(s.replayTail, marker)
	}
}

func (s *codexFoldState) continuationInput() []any {
	if s == nil {
		return nil
	}
	inputItems := make([]any, 0, len(s.originalInput)+len(s.replayTail))
	inputItems = append(inputItems, s.originalInput...)
	inputItems = append(inputItems, s.replayTail...)
	return inputItems
}

func (s *codexFoldState) commitBufferedItems(items []*codexBufferedItem) {
	if s == nil {
		return
	}
	for _, entry := range items {
		if item := codexBufferedDoneItem(entry); item != nil {
			s.committedOutput = append(s.committedOutput, codexOutputItem{OutputIndex: entry.outputIndex, Item: item})
		}
	}
}

func (s *codexFoldState) commitReasoningItems(items []map[string]any) {
	if s == nil {
		return
	}
	for _, item := range items {
		payload, err := json.Marshal(item)
		if err != nil {
			continue
		}
		if s.hasCommittedItemID(gjson.GetBytes(payload, "id").String()) {
			continue
		}
		s.committedOutput = append(s.committedOutput, codexOutputItem{OutputIndex: s.nextOutputIndex, Item: payload})
		s.nextOutputIndex++
	}
}

func (s *codexFoldState) hasCommittedItemID(id string) bool {
	if s == nil || id == "" {
		return false
	}
	for _, item := range s.committedOutput {
		if gjson.GetBytes(item.Item, "id").String() == id {
			return true
		}
	}
	return false
}

func (s *codexFoldState) hasLiveCommitBarrier(roundOut codexContinueFoldOutput) bool {
	for _, entry := range roundOut.liveItems {
		itemType := codexBufferedItemType(entry)
		if itemType == "function_call" || itemType == "custom_tool_call" {
			return true
		}
	}
	return false
}

func (u *codexUsage) add(other codexUsage) {
	if u == nil {
		return
	}
	u.InputTokens += other.InputTokens
	u.CachedTokens += other.CachedTokens
	u.OutputTokens += other.OutputTokens
	u.ReasoningTokens += other.ReasoningTokens
	u.TotalTokens += other.TotalTokens
}

func codexUsageFromMap(raw map[string]any) codexUsage {
	if len(raw) == 0 {
		return codexUsage{}
	}
	payload, err := json.Marshal(map[string]any{"response": map[string]any{"usage": raw}})
	if err != nil {
		return codexUsage{}
	}
	detail, _ := helps.ParseCodexUsage(payload)
	return codexUsageFromDetail(detail)
}

func codexUsageFromDetail(detail coreusage.Detail) codexUsage {
	return codexUsage{
		InputTokens:     detail.InputTokens + detail.CacheReadTokens,
		CachedTokens:    detail.CacheReadTokens,
		OutputTokens:    detail.OutputTokens,
		ReasoningTokens: detail.ReasoningTokens,
		TotalTokens:     detail.TotalTokens,
	}
}

func (u codexUsage) detail() coreusage.Detail {
	return coreusage.Detail{
		InputTokens:     u.InputTokens - u.CachedTokens,
		CacheReadTokens: u.CachedTokens,
		CachedTokens:    u.CachedTokens,
		OutputTokens:    u.OutputTokens,
		ReasoningTokens: u.ReasoningTokens,
		TotalTokens:     u.TotalTokens,
	}
}

func (s *codexFoldState) agentUsage(finalUsage codexUsage) codexUsage {
	if s == nil {
		return codexUsage{}
	}
	reasoningTokens := s.billedUsage.ReasoningTokens
	finalNonReasoning := finalUsage.OutputTokens - finalUsage.ReasoningTokens
	if finalNonReasoning < 0 {
		finalNonReasoning = 0
	}
	outputTokens := reasoningTokens + finalNonReasoning
	return codexUsage{
		InputTokens:     s.firstUsage.InputTokens,
		CachedTokens:    s.firstUsage.CachedTokens,
		OutputTokens:    outputTokens,
		ReasoningTokens: reasoningTokens,
		TotalTokens:     s.firstUsage.InputTokens + outputTokens,
	}
}

func codexBufferedItemType(entry *codexBufferedItem) string {
	if entry == nil {
		return ""
	}
	if item := codexBufferedDoneItem(entry); item != nil {
		return gjson.GetBytes(item, "type").String()
	}
	for _, line := range entry.lines {
		payload := codexDataPayload(line)
		if payload == nil {
			continue
		}
		if itemType := gjson.GetBytes(payload, "item.type").String(); itemType != "" {
			return itemType
		}
	}
	return ""
}

func codexBufferedDoneItem(entry *codexBufferedItem) []byte {
	if entry == nil {
		return nil
	}
	for i := len(entry.lines) - 1; i >= 0; i-- {
		payload := codexDataPayload(entry.lines[i])
		if payload == nil || gjson.GetBytes(payload, "type").String() != "response.output_item.done" {
			continue
		}
		item := gjson.GetBytes(payload, "item")
		if item.Exists() && item.IsObject() {
			return bytes.Clone([]byte(item.Raw))
		}
	}
	return nil
}

func codexCommitPolicy(cfg *config.CodexContinueConfig) codexOutputCommitPolicy {
	if cfg != nil && cfg.OutputCommitPolicy == string(codexCommitOnCleanTerminal) {
		return codexCommitOnCleanTerminal
	}
	return codexCommitLiveBarrier
}

// flushBufferedRound commits and forwards this round's buffered (message /
// unknown) items so a partial answer reaches the client before a synthetic
// incomplete or upstream error terminal. Live tool items are committed into
// fold state for terminal reconstruction but were already forwarded live.
func (fx *codexContinueFoldContext) flushBufferedRound(
	ctx context.Context,
	state *codexFoldState,
	roundOut codexContinueFoldOutput,
	identityState codexIdentityConfuseState,
	forwardEvent codexContinueForwardEvent,
) {
	if state == nil || forwardEvent == nil {
		return
	}
	state.commitBufferedItems(roundOut.liveItems)
	state.commitReasoningItems(roundOut.reasoningItems)
	state.commitBufferedItems(roundOut.bufferedItems)
	for _, entry := range roundOut.bufferedItems {
		lines := entry.lines
		if fx.cfg != nil && fx.cfg.RechunkFinalAnswer {
			lines = rechunkCodexBufferedMessage(lines, fx.cfg.RechunkSize)
		}
		for _, bufLine := range lines {
			forwardEvent(bufLine, identityState)
		}
	}
	if roundOut.terminalErr != nil && len(roundOut.bufferedItems) > 0 {
		helps.RecordAPIResponseError(ctx, fx.config(), fmt.Errorf("codex continue: flushed buffered output before terminal error: %w", roundOut.terminalErr))
	}
}

func (fx *codexContinueFoldContext) forwardFoldError(
	ctx context.Context,
	out chan<- cliproxyexecutor.StreamChunk,
	reporter *helps.UsageReporter,
	err error,
) {
	if err == nil {
		return
	}
	if reporter != nil {
		reporter.PublishFailure(ctx, err)
	}
	select {
	case out <- cliproxyexecutor.StreamChunk{Err: err}:
	case <-ctx.Done():
	}
}

func (fx *codexContinueFoldContext) codexFoldRoundNearContextLimit(roundOut codexContinueFoldOutput) bool {
	if fx == nil || fx.cfg == nil || fx.cfg.ContextLimitGuardRatio < 0 {
		return false
	}
	ratio := fx.cfg.ContextLimitGuardRatio
	if ratio == 0 {
		ratio = 0.95
	}
	usage := codexUsageFromMap(roundOut.usage)
	if usage.InputTokens <= 0 {
		return false
	}
	window := fx.codexFoldContextWindow()
	return window > 0 && float64(usage.InputTokens) >= float64(window)*ratio
}

func (fx *codexContinueFoldContext) codexFoldContextWindow() int {
	if fx == nil {
		return codexContinueDefaultContextWindow
	}
	model := fx.req.Model
	if model == "" {
		model = gjson.GetBytes(fx.baseBody, "model").String()
	}
	if info := registry.LookupModelInfo(model, "codex"); info != nil && info.ContextLength > 0 {
		return info.ContextLength
	}
	if info := registry.LookupModelInfo(model); info != nil && info.ContextLength > 0 {
		return info.ContextLength
	}
	return codexContinueDefaultContextWindow
}
