package executor

import (
	"encoding/json"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// buildSyntheticIncomplete constructs a response.incomplete event carrying
// proxy metadata about why the fold stopped. Used when continuation fails or
// upstream EOFs mid-stream.
func (fx *codexContinueFoldContext) buildSyntheticIncomplete(
	state *codexFoldState,
	roundOut codexContinueFoldOutput,
	reason string,
	roundNo int,
	totalOutputTokens int,
) []byte {
	resp := map[string]any{
		"status":             "incomplete",
		"incomplete_details": map[string]any{"reason": reason},
	}
	if len(roundOut.usage) > 0 {
		resp["usage"] = roundOut.usage
	}
	resp["metadata"] = map[string]any{
		"proxy_stopped_reason": reason,
		"proxy_round":          roundNo,
	}
	payload, _ := json.Marshal(map[string]any{
		"type":     "response.incomplete",
		"response": resp,
	})
	if state != nil {
		visibleID := state.responseIdentity.visibleResponseID
		if visibleID == "" {
			visibleID = roundOut.responseID
		}
		if visibleID != "" {
			payload, _ = sjson.SetBytes(payload, "response.id", visibleID)
		}
		payload, _ = sjson.SetRawBytes(payload, "response.output", []byte(`[]`))
		for _, output := range state.committedOutput {
			if len(output.Item) > 0 {
				payload, _ = sjson.SetRawBytes(payload, "response.output.-1", output.Item)
			}
		}
		payload = setCodexUsage(payload, "response.metadata.proxy_billed_usage", state.billedUsage)
		payload = setCodexRoundMetadata(payload, state)
		if state.responseIdentity.upstreamPreviousResponseID != "" {
			payload, _ = sjson.SetBytes(payload, "response.metadata.proxy_upstream_previous_response_id", state.responseIdentity.upstreamPreviousResponseID)
		}
	}
	formatted := append([]byte("data: "), payload...)
	return formatted
}

func (fx *codexContinueFoldContext) buildFoldedTerminal(
	state *codexFoldState,
	roundOut codexContinueFoldOutput,
	stoppedReason string,
) []byte {
	payload := codexDataPayload(roundOut.terminalEvent)
	if payload == nil {
		payload = []byte(`{"type":"response.completed","response":{"status":"completed"}}`)
	}
	terminal := payload
	if roundOut.terminalType == "" {
		terminal, _ = sjson.SetBytes(terminal, "type", "response.completed")
	}
	visibleID := state.responseIdentity.visibleResponseID
	if visibleID == "" {
		visibleID = roundOut.responseID
	}
	if visibleID != "" {
		terminal, _ = sjson.SetBytes(terminal, "response.id", visibleID)
	}
	if state.nextSequence >= 0 {
		terminal, _ = sjson.SetBytes(terminal, "sequence_number", state.nextSequence+1)
	}

	terminal, _ = sjson.SetRawBytes(terminal, "response.output", []byte(`[]`))
	for _, output := range state.committedOutput {
		if len(output.Item) > 0 {
			terminal, _ = sjson.SetRawBytes(terminal, "response.output.-1", output.Item)
		}
	}

	finalUsage := codexUsageFromMap(roundOut.usage)
	terminal = setCodexUsage(terminal, "response.usage", state.agentUsage(finalUsage))
	terminal = setCodexUsage(terminal, "response.metadata.proxy_billed_usage", state.billedUsage)
	terminal = setCodexRoundMetadata(terminal, state)
	if state.responseIdentity.upstreamPreviousResponseID != "" {
		terminal, _ = sjson.SetBytes(terminal, "response.metadata.proxy_upstream_previous_response_id", state.responseIdentity.upstreamPreviousResponseID)
	}
	if stoppedReason != "" {
		terminal, _ = sjson.SetBytes(terminal, "response.metadata.proxy_stopped_reason", stoppedReason)
	}
	return append([]byte("data: "), terminal...)
}

func setCodexUsage(payload []byte, path string, usage codexUsage) []byte {
	payload, _ = sjson.SetBytes(payload, path+".input_tokens", usage.InputTokens)
	if usage.CachedTokens > 0 {
		payload, _ = sjson.SetBytes(payload, path+".input_tokens_details.cached_tokens", usage.CachedTokens)
	}
	payload, _ = sjson.SetBytes(payload, path+".output_tokens", usage.OutputTokens)
	payload, _ = sjson.SetBytes(payload, path+".output_tokens_details.reasoning_tokens", usage.ReasoningTokens)
	payload, _ = sjson.SetBytes(payload, path+".total_tokens", usage.TotalTokens)
	return payload
}

func setCodexRoundMetadata(payload []byte, state *codexFoldState) []byte {
	if state == nil {
		return payload
	}
	payload, _ = sjson.SetRawBytes(payload, "response.metadata.proxy_rounds", []byte(`[]`))
	for _, round := range state.rounds {
		raw, err := json.Marshal(round)
		if err == nil {
			payload, _ = sjson.SetRawBytes(payload, "response.metadata.proxy_rounds.-1", raw)
		}
	}
	if len(state.responseIdentity.hiddenResponseIDs) > 0 {
		payload, _ = sjson.SetBytes(payload, "response.metadata.proxy_hidden_response_ids", state.responseIdentity.hiddenResponseIDs)
	}
	return payload
}

func codexRewriteLifecyclePayload(payload []byte, state *codexFoldState) []byte {
	if state == nil || len(payload) == 0 {
		return payload
	}
	eventType := gjson.GetBytes(payload, "type").String()
	switch eventType {
	case "response.created":
		if state.responseIdentity.visibleResponseID == "" {
			state.responseIdentity.visibleResponseID = gjson.GetBytes(payload, "response.id").String()
		}
	case "response.in_progress":
	default:
		return payload
	}
	visibleID := state.responseIdentity.visibleResponseID
	if visibleID != "" {
		payload, _ = sjson.SetBytes(payload, "response.id", visibleID)
	}
	state.nextSequence++
	payload, _ = sjson.SetBytes(payload, "sequence_number", state.nextSequence)
	return payload
}
