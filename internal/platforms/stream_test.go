package platforms

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeOpenAIStreamPreservesToolsReasoningUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"reasoning_content":"thinking","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":2}}}`+"\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	var chunks []string
	err := streamOpenAI(context.Background(), server.Client(), server.URL, make(http.Header), map[string]any{}, func(raw json.RawMessage) error { chunks = append(chunks, string(raw)); return nil })
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(chunks, "\n")
	for _, want := range []string{`"type":"reasoning-delta"`, `"id":"call-1"`, `"inputTokens":10`, `"cacheReadTokens":2`, `"kind":"tool-calls"`} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %s: %s", want, all)
		}
	}
}
func TestNativeAnthropicStreamAndTruncation(t *testing.T) {
	for _, complete := range []bool{true, false} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			events := []string{
				`{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":3}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c1","name":"run"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`,
			}
			if complete {
				events = append(events, `{"type":"message_stop"}`)
			}
			for _, e := range events {
				fmt.Fprintf(w, "data: %s\n\n", e)
			}
		}))
		var chunks []string
		err := streamAnthropic(context.Background(), server.Client(), server.URL, make(http.Header), map[string]any{}, func(raw json.RawMessage) error { chunks = append(chunks, string(raw)); return nil })
		server.Close()
		if !complete {
			if err == nil {
				t.Fatal("truncated stream accepted as success")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		all := strings.Join(chunks, "\n")
		for _, want := range []string{`"id":"c1"`, `"argumentsDelta":"{}"`, `"cacheReadTokens":3`, `"kind":"tool-calls"`} {
			if !strings.Contains(all, want) {
				t.Fatalf("missing %s: %s", want, all)
			}
		}
	}
}
func TestAnthropicProjectionKeepsToolHistory(t *testing.T) {
	body := map[string]any{"model": "m", "messages": []any{map[string]any{"role": "system", "content": "rules"}, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1", "function": map[string]any{"name": "lookup", "arguments": "{}"}}}}, map[string]any{"role": "tool", "tool_call_id": "c1", "content": "answer"}}}
	projected, err := toAnthropic(body)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(jsonBytes(projected))
	for _, want := range []string{`"system"`, `"type":"tool_use"`, `"tool_use_id":"c1"`, `"text":"answer"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("missing %s: %s", want, raw)
		}
	}
}
