package executor

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestFinalizeCodexWebsocketRequest_StripsRealMsgIDsOnFullTranscript(t *testing.T) {
	finalized := finalizeCodexWebsocketRequest(
		&config.Config{},
		nil,
		codexFullTranscriptItemIDsFixture(),
		codexFullTranscriptItemIDsFixture(),
		nil,
		"test-session",
		http.Header{},
		codexClientMetadataHeaders{},
	)
	assertNoRealMsgIDs(t, finalized.body)
}

func TestFinalizeCodexWebsocketRequest_KeepsRealMsgIDsWhenPreviousResponseIDSet(t *testing.T) {
	chained := codexFullTranscriptItemIDsChainedFixture()
	finalized := finalizeCodexWebsocketRequest(
		&config.Config{},
		nil,
		chained,
		chained,
		nil,
		"test-session",
		http.Header{},
		codexClientMetadataHeaders{},
	)
	if !bytes.Contains(finalized.body, []byte(`"id":"msg_0b17keepthisid"`)) {
		t.Fatalf("chained finalize stripped real msg id; body=%s", string(finalized.body))
	}
}
