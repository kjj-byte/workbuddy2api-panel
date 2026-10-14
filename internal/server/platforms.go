package server

import (
	"context"
	"encoding/json"
	"github.com/linguo2625469/workbuddy2api-panel/internal/platforms"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
	"net/http"
	"time"
)

type PlatformBackend interface {
	platforms.Caller
	Models(context.Context) ([]map[string]any, error)
}

func isPlatformProvider(id string) bool {
	switch id {
	case "codearts", "lobsterai", "qoder", "qodercn", "trae", "loomy", "minimax":
		return true
	}
	return false
}

type observedPlatform struct {
	base     platforms.Caller
	recorder *usage.Recorder
	stat     *chatStat
	provider string
}

func (o observedPlatform) Call(ctx context.Context, op map[string]any, receive func(json.RawMessage) error) (json.RawMessage, error) {
	delta := usage.Delta{}
	raw, err := o.base.Call(ctx, op, func(chunk json.RawMessage) error {
		var event struct {
			Type  string `json:"type"`
			Usage struct {
				Input      int64 `json:"inputTokens"`
				Output     int64 `json:"outputTokens"`
				CacheRead  int64 `json:"cacheReadTokens"`
				CacheWrite int64 `json:"cacheWriteTokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(chunk, &event) == nil {
			if o.stat.ttfb == 0 && (event.Type == "text-delta" || event.Type == "reasoning-delta" || event.Type == "tool-call-delta") {
				o.stat.ttfb = time.Since(o.stat.start)
			}
			if event.Type == "usage" {
				delta.HasPromptTokens = true
				delta.HasCompletion = true
				delta.HasTotal = true
				delta.HasCacheTokens = true
				delta.PromptTokens = event.Usage.Input + event.Usage.CacheRead + event.Usage.CacheWrite
				delta.CompletionTokens = event.Usage.Output
				delta.TotalTokens = delta.PromptTokens + delta.CompletionTokens
				delta.CacheHitTokens = event.Usage.CacheRead
				delta.CacheMissTokens = delta.PromptTokens - delta.CacheHitTokens
				o.stat.promptTokens = delta.PromptTokens
				o.stat.completionTokens = delta.CompletionTokens
				o.stat.totalTokens = delta.TotalTokens
				o.stat.cacheHit = delta.CacheHitTokens
				o.stat.cacheMiss = delta.CacheMissTokens
				o.stat.hasCache = true
				o.stat.toks = int(delta.CompletionTokens)
			}
		}
		return receive(chunk)
	})
	elapsed := time.Since(o.stat.start)
	delta.HasLatency = true
	delta.LatencyMs = elapsed.Milliseconds()
	if speed, ok := tokensPerSecond(delta.CompletionTokens, elapsed, o.stat.ttfb); ok {
		delta.HasTPS = true
		delta.TokensPerSecond = speed
	}
	o.stat.status = 200
	o.stat.outcome = "success"
	if err != nil {
		o.stat.status = 502
		o.stat.outcome = "upstream_error"
	}
	// The private adapter owns account selection. Host statistics deliberately identify
	// the provider pool; its own token ledger retains the actual per-account breakdown.
	if o.recorder != nil {
		o.recorder.Add(time.Now(), o.provider, o.stat.uid, o.stat.model, delta, err == nil)
	}
	return raw, err
}
func (h *Handler) platformChat(w http.ResponseWriter, r *http.Request, body []byte, provider string, stream bool) {
	stat := newChatStat(time.Now(), body, stream)
	stat.uid = "platform:" + provider
	stat.nick = provider + " 账号池"
	stat.attempts = 1
	if trace := requestTraceFrom(r); trace != nil {
		trace.stat = stat
		stat.requestID = trace.id
		stat.clientIP, stat.userAgent = trace.clientIP, trace.userAgent
	}
	defer stat.done()
	platforms.Chat(r.Context(), observedPlatform{h.cfg.Platforms, h.cfg.Usage, stat, provider}, w, body)
}
