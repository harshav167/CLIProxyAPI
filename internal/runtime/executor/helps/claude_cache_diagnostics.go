package helps

import (
	"bytes"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const claudeCacheDiagnosticsTTL = time.Hour
const claudeCacheDiagnosticsCleanupPeriod = 15 * time.Minute

type claudeCacheDiagnosticsEntry struct {
	messageID string
	expire    time.Time
}

var (
	claudeCacheDiagnosticsMu    sync.Mutex
	claudeCacheDiagnosticsStore = make(map[string]claudeCacheDiagnosticsEntry)
	claudeCacheDiagnosticsOnce  sync.Once
)

// PrepareClaudeCacheDiagnostics adds Anthropic cache diagnostics to adaptive
// Opus requests. The first request sends an explicit null; later requests in
// the same execution session reference the prior upstream message ID.
func PrepareClaudeCacheDiagnostics(payload []byte, sessionID string) []byte {
	if !claudeCacheDiagnosticsEligible(payload) {
		return payload
	}
	if previousMessageID := strings.TrimSpace(gjson.GetBytes(payload, "diagnostics.previous_message_id").String()); previousMessageID != "" {
		return payload
	}
	previousMessageID := claudeCacheDiagnosticsPreviousMessageID(sessionID, time.Now())
	if previousMessageID == "" {
		updated, err := sjson.SetRawBytes(payload, "diagnostics.previous_message_id", []byte("null"))
		if err != nil {
			return payload
		}
		return updated
	}
	updated, err := sjson.SetBytes(payload, "diagnostics.previous_message_id", previousMessageID)
	if err != nil {
		return payload
	}
	return updated
}

// RecordClaudeCacheDiagnosticsMessageID stores the upstream Claude message ID
// for the next request in the same execution session.
func RecordClaudeCacheDiagnosticsMessageID(sessionID, messageID string) {
	sessionID = strings.TrimSpace(sessionID)
	messageID = strings.TrimSpace(messageID)
	if sessionID == "" || messageID == "" {
		return
	}
	claudeCacheDiagnosticsOnce.Do(startClaudeCacheDiagnosticsCleanup)
	claudeCacheDiagnosticsMu.Lock()
	claudeCacheDiagnosticsStore[sessionID] = claudeCacheDiagnosticsEntry{
		messageID: messageID,
		expire:    time.Now().Add(claudeCacheDiagnosticsTTL),
	}
	claudeCacheDiagnosticsMu.Unlock()
}

// ClaudeMessageIDFromResponse extracts a Claude message ID from either a
// non-stream response body or a message_start SSE line.
func ClaudeMessageIDFromResponse(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	data := JSONPayload(payload)
	if len(data) == 0 {
		data = payload
	}
	eventType := strings.TrimSpace(gjson.GetBytes(data, "type").String())
	if eventType == "message" && bytes.Equal(data, payload) {
		return strings.TrimSpace(gjson.GetBytes(data, "id").String())
	}
	if eventType != "message_start" {
		return ""
	}
	return strings.TrimSpace(gjson.GetBytes(data, "message.id").String())
}

func ClaudeMessageIDFromSSE(payload []byte) string {
	for _, line := range bytes.Split(payload, []byte("\n")) {
		if messageID := ClaudeMessageIDFromResponse(line); messageID != "" {
			return messageID
		}
	}
	return ""
}

func claudeCacheDiagnosticsEligible(payload []byte) bool {
	return strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, "thinking.type").String()), "adaptive") &&
		registry.IsAdaptiveClaudeOpusModel(gjson.GetBytes(payload, "model").String())
}

func claudeCacheDiagnosticsPreviousMessageID(sessionID string, now time.Time) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	claudeCacheDiagnosticsMu.Lock()
	defer claudeCacheDiagnosticsMu.Unlock()
	entry, ok := claudeCacheDiagnosticsStore[sessionID]
	if !ok {
		return ""
	}
	if !entry.expire.After(now) {
		delete(claudeCacheDiagnosticsStore, sessionID)
		return ""
	}
	return entry.messageID
}

func startClaudeCacheDiagnosticsCleanup() {
	go func() {
		ticker := time.NewTicker(claudeCacheDiagnosticsCleanupPeriod)
		defer ticker.Stop()
		for now := range ticker.C {
			purgeExpiredClaudeCacheDiagnostics(now)
		}
	}()
}

func purgeExpiredClaudeCacheDiagnostics(now time.Time) {
	claudeCacheDiagnosticsMu.Lock()
	for key, entry := range claudeCacheDiagnosticsStore {
		if !entry.expire.After(now) {
			delete(claudeCacheDiagnosticsStore, key)
		}
	}
	claudeCacheDiagnosticsMu.Unlock()
}

func resetClaudeCacheDiagnosticsStore() {
	claudeCacheDiagnosticsMu.Lock()
	clear(claudeCacheDiagnosticsStore)
	claudeCacheDiagnosticsMu.Unlock()
}
