package responses

import (
	"context"
	"testing"
)

func TestConvertCodexResponseToOpenAIResponsesNonStreamIncomplete(t *testing.T) {
	// Given
	raw := []byte(`{"type":"response.incomplete","response":{"id":"resp_123","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}}`)

	// When
	got := ConvertCodexResponseToOpenAIResponsesNonStream(context.Background(), "gpt-5.5", nil, nil, raw, nil)

	// Then
	want := `{"id":"resp_123","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`
	if string(got) != want {
		t.Fatalf("incomplete response = %s, want %s", got, want)
	}
}
