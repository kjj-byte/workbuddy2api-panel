package platforms

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

type Caller interface {
	Call(context.Context, map[string]any, func(json.RawMessage) error) (json.RawMessage, error)
}
type tool struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Chat preserves tool calls, reasoning, usage and streaming through native adapters.
func Chat(ctx context.Context, caller Caller, w http.ResponseWriter, body []byte) {
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	var openAI map[string]any
	if json.Unmarshal(body, &req) != nil || json.Unmarshal(body, &openAI) != nil {
		writeError(w, 400, "invalid_request", "invalid JSON")
		return
	}
	id := fmt.Sprintf("chatcmpl-platform-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	text, reasoning, finish := "", "", "stop"
	tools := map[int]*tool{}
	streamed := map[int]bool{}
	var usage any
	started := false
	emit := func(delta any, reason any) error {
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			started = true
		}
		chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}}
		if reason != nil && usage != nil {
			chunk["usage"] = usage
		}
		raw, _ := json.Marshal(chunk)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return err
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return nil
	}
	_, err := caller.Call(ctx, map[string]any{"type": "complete", "request": map[string]any{"model": req.Model, "openAi": openAI}}, func(raw json.RawMessage) error {
		var c struct {
			Type           string `json:"type"`
			Index          int    `json:"index"`
			Text           string `json:"text"`
			ID             string `json:"id"`
			Name           string `json:"name"`
			ArgumentsDelta string `json:"argumentsDelta"`
			Block          struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"block"`
			Usage struct {
				Input      int `json:"inputTokens"`
				Output     int `json:"outputTokens"`
				CacheRead  int `json:"cacheReadTokens"`
				CacheWrite int `json:"cacheWriteTokens"`
				Reasoning  int `json:"reasoningTokens"`
			} `json:"usage"`
			Reason struct {
				Kind    string `json:"kind"`
				Failure struct {
					Message string `json:"message"`
				} `json:"failure"`
			} `json:"reason"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
		var delta map[string]any
		switch c.Type {
		case "text-delta":
			text += c.Text
			delta = map[string]any{"content": c.Text}
		case "reasoning-delta":
			reasoning += c.Text
			delta = map[string]any{"reasoning_content": c.Text}
		case "tool-call-delta", "block-end":
			if c.Type == "block-end" && c.Block.Type != "tool-call" {
				return nil
			}
			t := tools[c.Index]
			if t == nil {
				t = &tool{Type: "function"}
				tools[c.Index] = t
			}
			if c.Type == "tool-call-delta" {
				if c.ID != "" {
					t.ID = c.ID
				}
				if c.Name != "" {
					t.Function.Name = c.Name
				}
				t.Function.Arguments += c.ArgumentsDelta
				fn := map[string]any{"arguments": c.ArgumentsDelta}
				tc := map[string]any{"index": c.Index, "function": fn}
				if c.ID != "" {
					tc["id"] = c.ID
					tc["type"] = "function"
				}
				if c.Name != "" {
					fn["name"] = c.Name
				}
				delta = map[string]any{"tool_calls": []any{tc}}
				streamed[c.Index] = true
			} else {
				if c.Block.ID != "" {
					t.ID = c.Block.ID
				}
				if c.Block.Name != "" {
					t.Function.Name = c.Block.Name
				}
				t.Function.Arguments = c.Block.Arguments
				if !streamed[c.Index] {
					delta = map[string]any{"tool_calls": []any{map[string]any{"index": c.Index, "id": t.ID, "type": "function", "function": t.Function}}}
				}
			}
		case "usage":
			input := c.Usage.Input + c.Usage.CacheRead + c.Usage.CacheWrite
			usage = map[string]any{"prompt_tokens": input, "completion_tokens": c.Usage.Output, "total_tokens": input + c.Usage.Output, "prompt_tokens_details": map[string]any{"cached_tokens": c.Usage.CacheRead}, "completion_tokens_details": map[string]any{"reasoning_tokens": c.Usage.Reasoning}}
		case "finish":
			switch c.Reason.Kind {
			case "max-tokens":
				finish = "length"
			case "tool-calls", "tool-call":
				finish = "tool_calls"
			case "error", "aborted":
				return fmt.Errorf("platform generation failed: %s", c.Reason.Failure.Message)
			}
		}
		if req.Stream && delta != nil {
			return emit(delta, nil)
		}
		return nil
	})
	if err != nil {
		status := 502
		if re, ok := err.(*RemoteError); ok && re.Status >= 400 && re.Status <= 599 {
			status = re.Status
		}
		if started {
			raw, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		} else {
			writeError(w, status, "upstream_error", err.Error())
		}
		return
	}
	if len(tools) > 0 && finish == "stop" {
		finish = "tool_calls"
	}
	if req.Stream {
		if err := emit(map[string]any{}, finish); err == nil {
			fmt.Fprint(w, "data: [DONE]\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		return
	}
	message := map[string]any{"role": "assistant", "content": text}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(tools) > 0 {
		indexes := make([]int, 0, len(tools))
		for i := range tools {
			indexes = append(indexes, i)
		}
		sort.Ints(indexes)
		all := make([]*tool, 0, len(tools))
		for _, i := range indexes {
			all = append(all, tools[i])
		}
		message["tool_calls"] = all
	}
	result := map[string]any{"id": id, "object": "chat.completion", "created": created, "model": req.Model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}}
	if usage != nil {
		result["usage"] = usage
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": typ, "message": msg}})
}
