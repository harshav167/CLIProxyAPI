package helps

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// StripCodexFullTranscriptServerMessageIDs strips real server-minted message ids from
// Responses full-transcript requests (no previous_response_id). Droid replays
// assistant history with message ids kept but reasoning ids dropped, which
// upstream rejects as "message without its required reasoning item". Stripping
// the real server message ids (keeping msg_synth_* and all other item types)
// cleared both that pair error and the duplicate-id error on the measured
// Droid full-transcript fixture; it is not a guarantee for every possible body.
//
// Leaves body unchanged when previous_response_id is non-empty after TrimSpace
// (incremental/chained mode relies on stable ids).
func StripCodexFullTranscriptServerMessageIDs(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	if strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != "" {
		return body
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body
	}
	input.ForEach(func(key, value gjson.Result) bool {
		id := value.Get("id")
		if !id.Exists() || id.Type != gjson.String {
			return true
		}
		idStr := id.String()
		if !strings.HasPrefix(idStr, "msg_") || strings.HasPrefix(idStr, "msg_synth_") {
			return true
		}
		path := fmt.Sprintf("input.%d.id", key.Int())
		updated, err := sjson.DeleteBytes(body, path)
		if err != nil {
			return true
		}
		body = updated
		return true
	})
	return body
}
