package helps

import (
	"strings"
	"testing"
)

func TestNormalizeOpenAICompatStreamLine_AlibabaShapeDropsEmptiesAndRestoresRole(t *testing.T) {
	// Alibaba MaaS emits empty-string fields on every chunk and only sends
	// role:"assistant" on the first chunk. Cursor needs the leaner shape below.
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "reasoning-phase chunk: empty content dropped",
			in:   `data: {"id":"x","choices":[{"index":0,"delta":{"reasoning_content":"The","content":""},"finish_reason":null}]}`,
			want: `data: {"id":"x","choices":[{"index":0,"delta":{"reasoning_content":"The"},"finish_reason":null}]}`,
		},
		{
			name: "content-phase chunk: empty reasoning_content dropped, role restored",
			in:   `data: {"id":"x","choices":[{"index":0,"delta":{"content":"Hello","reasoning_content":""},"finish_reason":null}]}`,
			want: `data: {"id":"x","choices":[{"index":0,"delta":{"content":"Hello","role":"assistant"},"finish_reason":null}]}`,
		},
		{
			name: "first-chunk role presence is preserved when content empty",
			in:   `data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":""},"finish_reason":null}]}`,
			want: `data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		},
		{
			name: "terminal chunk with empty content+reasoning_content is cleaned",
			in:   `data: {"id":"x","choices":[{"index":0,"delta":{"content":"","reasoning_content":""},"finish_reason":"stop"}]}`,
			want: `data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(NormalizeOpenAICompatStreamLine([]byte(c.in)))
			if got != c.want {
				t.Fatalf("mismatch\n in:  %s\n got: %s\n want:%s", c.in, got, c.want)
			}
		})
	}
}

func TestNormalizeOpenAICompatStreamLine_OllamaShapeIsNoOp(t *testing.T) {
	in := `data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello","reasoning":" No need for tools."},"finish_reason":null}]}`
	got := string(NormalizeOpenAICompatStreamLine([]byte(in)))
	if got != in {
		t.Fatalf("expected no-op on Ollama shape\n in:  %s\n got: %s", in, got)
	}
}

func TestNormalizeOpenAICompatStreamLine_NonDataAndDoneAreNoOp(t *testing.T) {
	cases := []string{
		``,
		`: comment line`,
		`data: [DONE]`,
		`data: `,
		`event: ping`,
	}
	for _, in := range cases {
		got := string(NormalizeOpenAICompatStreamLine([]byte(in)))
		if got != in {
			t.Fatalf("expected no-op on %q; got %q", in, got)
		}
	}
}

func TestNormalizeOpenAICompatStreamLine_PreservesFramingWhenAlreadyClean(t *testing.T) {
	in := `data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello","reasoning_content":"thinking"},"finish_reason":null}]}`
	got := string(NormalizeOpenAICompatStreamLine([]byte(in)))
	if got != in {
		t.Fatalf("expected byte-identical passthrough\n in: %s\n got:%s", in, got)
	}
	raw := `{"id":"x","choices":[{"index":0,"delta":{"content":"Hi","reasoning_content":""}}]}`
	out := string(NormalizeOpenAICompatStreamLine([]byte(raw)))
	if !strings.Contains(out, `"role":"assistant"`) || strings.Contains(out, `"reasoning_content":""`) {
		t.Fatalf("raw-JSON path did not normalise correctly: %s", out)
	}
}
