package platforms

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func openStream(ctx context.Context, client *http.Client, url string, h http.Header, body any) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBytes(body)))
	if err != nil {
		return nil, err
	}
	req.Header = h
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, &RemoteError{fmt.Sprintf("推理端点返回 HTTP %d", resp.StatusCode), resp.StatusCode}
	}
	return resp, nil
}
func readSSE(body io.Reader, consume func(string) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 65536), 16<<20)
	var data []string
	dispatch := func() error {
		if len(data) == 0 {
			return nil
		}
		value := strings.Join(data, "\n")
		data = nil
		return consume(value)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return dispatch()
}
func streamOpenAI(ctx context.Context, client *http.Client, url string, h http.Header, body map[string]any, receive func(json.RawMessage) error) error {
	body["stream"] = true
	body["stream_options"] = map[string]any{"include_usage": true}
	resp, err := openStream(ctx, client, url, h, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return consumeOpenAIResponse(resp, receive)
}
func consumeOpenAIResponse(resp *http.Response, receive func(json.RawMessage) error) error {
	var err error
	reason := "stop"
	ended := false
	seen := false
	consume := func(data string) error {
		if data == "" || data == "[DONE]" {
			ended = true
			return nil
		}
		var item map[string]any
		if json.Unmarshal([]byte(data), &item) != nil {
			return fmt.Errorf("invalid OpenAI event")
		}
		if item["error"] != nil {
			return fmt.Errorf("平台推理返回错误")
		}
		if code := str(item["error_code"]); code != "" {
			return fmt.Errorf("平台推理返回错误: %s", code)
		}
		if u, ok := item["usage"].(map[string]any); ok {
			prompt, _ := number(u["prompt_tokens"])
			output, _ := number(u["completion_tokens"])
			cached, _ := number(obj(u["prompt_tokens_details"])["cached_tokens"])
			thinking, _ := number(obj(u["completion_tokens_details"])["reasoning_tokens"])
			if err := emitChunk(receive, map[string]any{"type": "usage", "usage": map[string]any{"inputTokens": prompt - cached, "cacheReadTokens": cached, "outputTokens": output, "reasoningTokens": thinking}}); err != nil {
				return err
			}
		}
		for _, row := range list(item["choices"]) {
			choice := obj(row)
			delta := obj(choice["delta"])
			if text := str(delta["content"]); text != "" {
				seen = true
				if err := emitChunk(receive, map[string]any{"type": "text-delta", "text": text}); err != nil {
					return err
				}
			}
			thinking := str(delta["reasoning_content"])
			if thinking == "" {
				thinking = str(delta["reasoning"])
			}
			if thinking != "" {
				seen = true
				if err := emitChunk(receive, map[string]any{"type": "reasoning-delta", "text": thinking}); err != nil {
					return err
				}
			}
			for _, entry := range list(delta["tool_calls"]) {
				tool := obj(entry)
				fn := obj(tool["function"])
				index, _ := number(tool["index"])
				seen = true
				if err := emitChunk(receive, map[string]any{"type": "tool-call-delta", "index": int(index), "id": str(tool["id"]), "name": str(fn["name"]), "argumentsDelta": str(fn["arguments"])}); err != nil {
					return err
				}
			}
			if finish := str(choice["finish_reason"]); finish != "" {
				ended = true
				switch finish {
				case "length":
					reason = "max-tokens"
				case "tool_calls":
					reason = "tool-calls"
				}
			}
		}
		return nil
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		// Some upstreams mislabel SSE as JSON; sniff the actual bytes.
		reader := bufio.NewReader(resp.Body)
		prefix, _ := reader.Peek(5)
		if string(prefix) == "data:" {
			err = readSSE(reader, consume)
		} else {
			return fmt.Errorf("平台未返回请求的流式响应")
		}
	} else {
		err = readSSE(resp.Body, consume)
	}
	if err != nil {
		return err
	}
	if !ended {
		return fmt.Errorf("平台流未正常结束")
	}
	if !seen {
		return fmt.Errorf("平台返回空响应")
	}
	return emitChunk(receive, map[string]any{"type": "finish", "reason": map[string]any{"kind": reason}})
}
func streamAnthropic(ctx context.Context, client *http.Client, url string, h http.Header, body any, receive func(json.RawMessage) error) error {
	resp, err := openStream(ctx, client, url, h, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	input, output, cached, write := float64(0), float64(0), float64(0), float64(0)
	reason := "stop"
	ended, seen := false, false
	err = readSSE(resp.Body, func(data string) error {
		var e map[string]any
		if json.Unmarshal([]byte(data), &e) != nil {
			return fmt.Errorf("invalid Anthropic event")
		}
		index, _ := number(e["index"])
		switch str(e["type"]) {
		case "error":
			return fmt.Errorf("平台推理返回错误")
		case "message_start":
			u := obj(obj(e["message"])["usage"])
			input, _ = number(u["input_tokens"])
			cached, _ = number(u["cache_read_input_tokens"])
			write, _ = number(u["cache_creation_input_tokens"])
		case "content_block_start":
			block := obj(e["content_block"])
			if str(block["type"]) == "tool_use" {
				seen = true
				return emitChunk(receive, map[string]any{"type": "tool-call-delta", "index": int(index), "id": str(block["id"]), "name": str(block["name"]), "argumentsDelta": ""})
			}
		case "content_block_delta":
			delta := obj(e["delta"])
			switch str(delta["type"]) {
			case "text_delta":
				seen = true
				return emitChunk(receive, map[string]any{"type": "text-delta", "text": str(delta["text"])})
			case "thinking_delta":
				seen = true
				return emitChunk(receive, map[string]any{"type": "reasoning-delta", "text": str(delta["thinking"])})
			case "input_json_delta":
				seen = true
				return emitChunk(receive, map[string]any{"type": "tool-call-delta", "index": int(index), "argumentsDelta": str(delta["partial_json"])})
			}
		case "message_delta":
			u := obj(e["usage"])
			if n, ok := number(u["output_tokens"]); ok {
				output = n
			}
			switch str(obj(e["delta"])["stop_reason"]) {
			case "max_tokens":
				reason = "max-tokens"
			case "tool_use":
				reason = "tool-calls"
			}
		case "message_stop":
			ended = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !ended || !seen {
		return fmt.Errorf("平台响应为空或未正常结束")
	}
	if err = emitChunk(receive, map[string]any{"type": "usage", "usage": map[string]any{"inputTokens": input, "outputTokens": output, "cacheReadTokens": cached, "cacheWriteTokens": write}}); err != nil {
		return err
	}
	return emitChunk(receive, map[string]any{"type": "finish", "reason": map[string]any{"kind": reason}})
}

func toAnthropic(body map[string]any) (map[string]any, error) {
	result := map[string]any{"model": body["model"], "stream": true, "max_tokens": 8192}
	if n, ok := body["max_completion_tokens"]; ok {
		result["max_tokens"] = n
	} else if n, ok := body["max_tokens"]; ok {
		result["max_tokens"] = n
	}
	var messages []any
	var system []any
	appendMessage := func(role string, blocks []any) {
		if len(blocks) == 0 {
			return
		}
		if len(messages) > 0 {
			last := obj(messages[len(messages)-1])
			if last["role"] == role {
				last["content"] = append(list(last["content"]), blocks...)
				return
			}
		}
		messages = append(messages, map[string]any{"role": role, "content": blocks})
	}
	for _, row := range list(body["messages"]) {
		m := obj(row)
		role := str(m["role"])
		var blocks []any
		if text, ok := m["content"].(string); ok && text != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": text})
		} else {
			for _, part := range list(m["content"]) {
				b := obj(part)
				switch str(b["type"]) {
				case "text":
					blocks = append(blocks, b)
				case "image_url":
					u := str(obj(b["image_url"])["url"])
					head, data, ok := strings.Cut(u, ",")
					if !ok || !strings.HasPrefix(head, "data:image/") || !strings.HasSuffix(head, ";base64") {
						return nil, fmt.Errorf("此平台图片需使用内联 data URL")
					}
					media := strings.TrimSuffix(strings.TrimPrefix(head, "data:"), ";base64")
					blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": media, "data": data}})
				}
			}
		}
		if role == "system" || role == "developer" {
			system = append(system, blocks...)
			continue
		}
		if role == "tool" {
			appendMessage("user", []any{map[string]any{"type": "tool_result", "tool_use_id": m["tool_call_id"], "content": blocks}})
			continue
		}
		for _, call := range list(m["tool_calls"]) {
			c := obj(call)
			fn := obj(c["function"])
			var input any
			if json.Unmarshal([]byte(str(fn["arguments"])), &input) != nil {
				return nil, fmt.Errorf("工具参数不是有效 JSON")
			}
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": c["id"], "name": fn["name"], "input": input})
		}
		if role != "assistant" {
			role = "user"
		}
		appendMessage(role, blocks)
	}
	result["messages"] = messages
	if len(system) > 0 {
		result["system"] = system
	}
	var tools []any
	for _, entry := range list(body["tools"]) {
		fn := obj(obj(entry)["function"])
		if str(fn["name"]) != "" {
			tools = append(tools, map[string]any{"name": fn["name"], "description": fn["description"], "input_schema": fn["parameters"]})
		}
	}
	if len(tools) > 0 {
		result["tools"] = tools
	}
	if t, ok := body["temperature"]; ok {
		result["temperature"] = t
	}
	if stop, ok := body["stop"]; ok {
		if s, ok := stop.(string); ok {
			result["stop_sequences"] = []string{s}
		} else {
			result["stop_sequences"] = stop
		}
	}
	return result, nil
}
