package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakePlatforms struct {
	calls   int
	request map[string]any
	chunks  []string
}

func (f *fakePlatforms) Models(context.Context) ([]map[string]any, error) {
	return []map[string]any{{"id": "qoder/test", "object": "model"}}, nil
}
func (f *fakePlatforms) Call(_ context.Context, op map[string]any, onChunk func(json.RawMessage) error) (json.RawMessage, error) {
	f.calls++
	f.request = op
	chunks := f.chunks
	if chunks == nil {
		chunks = []string{`{"type":"text-delta","text":"OK"}`, `{"type":"finish","reason":{"kind":"stop"}}`}
	}
	for _, c := range chunks {
		if err := onChunk(json.RawMessage(c)); err != nil {
			return nil, err
		}
	}
	return nil, nil
}
func TestPlatformRoutesAcrossProtocols(t *testing.T) {
	for _, tc := range []struct{ path, body, want string }{
		{"/v1/chat/completions", `{"model":"qoder/test","messages":[{"role":"user","content":"hi"}]}`, `"content":"OK"`},
		{"/v1/responses", `{"model":"qoder/test","input":"hi"}`, `"output_text"`},
		{"/v1/responses", `{"model":"qoder/test","input":"hi","stream":true}`, `response.completed`},
		{"/v1/messages", `{"model":"qoder/test","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`, `"text":"OK"`},
	} {
		t.Run(tc.path+tc.want, func(t *testing.T) {
			backend := &fakePlatforms{}
			h := NewHandler(Config{Platforms: backend, APIKey: "test-key"})
			r := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer test-key")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if backend.calls != 1 {
				t.Fatal("request did not reach platform")
			}
		})
	}
}
func TestPlatformAuthenticationAndDisabled(t *testing.T) {
	backend := &fakePlatforms{}
	h := NewHandler(Config{Platforms: backend, APIKey: "secret"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"qoder/test"}`)))
	if w.Code != 401 || backend.calls != 0 {
		t.Fatal("platform bypassed authentication")
	}
	h = NewHandler(Config{})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"qoder/test"}`)))
	if w.Code != 503 {
		t.Fatalf("disabled platform status %d", w.Code)
	}
}
func TestPlatformToolsAndUsage(t *testing.T) {
	backend := &fakePlatforms{chunks: []string{
		`{"type":"reasoning-delta","text":"think"}`,
		`{"type":"tool-call-delta","index":1,"id":"call-1","name":"lookup","argumentsDelta":"{\"q\":"}`,
		`{"type":"tool-call-delta","index":1,"argumentsDelta":"\"x\"}"}`,
		`{"type":"block-end","index":1,"block":{"type":"tool-call","id":"call-1","name":"lookup","arguments":"{\"q\":\"x\"}"}}`,
		`{"type":"usage","usage":{"inputTokens":10,"cacheReadTokens":4,"outputTokens":6,"reasoningTokens":2}}`,
		`{"type":"finish","reason":{"kind":"tool-calls"}}`,
	}}
	for _, stream := range []bool{false, true} {
		h := NewHandler(Config{Platforms: backend})
		body := `{"model":"qoder/test","messages":[]}`
		if stream {
			body = `{"model":"qoder/test","messages":[],"stream":true}`
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		for _, want := range []string{`"tool_calls"`, `"prompt_tokens":14`, `"reasoning_tokens":2`, `"reasoning_content":"think"`} {
			if !strings.Contains(w.Body.String(), want) {
				t.Fatalf("missing %s: %s", want, w.Body.String())
			}
		}
		if stream && strings.Count(w.Body.String(), `"id":"call-1"`) != 1 {
			t.Fatal("tool call emitted twice")
		}
	}
}
