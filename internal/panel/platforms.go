package panel

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"time"
)

//go:embed platforms.js
var platformsJS []byte

func (p *Panel) platformScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(platformsJS)
}
func (p *Panel) platformState(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Platforms == nil {
		writeJSON(w, 200, map[string]any{"enabled": false, "providers": []any{}})
		return
	}
	raw, err := p.cfg.Platforms.Call(r.Context(), map[string]any{"type": "scheduleState"}, nil)
	if err != nil {
		writeErr(w, 503, err.Error())
		return
	}
	var state map[string]any
	_ = json.Unmarshal(raw, &state)
	state["available"] = true
	writeJSON(w, 200, state)
}
func platformPayload(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	decoder := json.NewDecoder(r.Body)
	if decoder.Decode(dst) != nil {
		writeErr(w, 400, "invalid JSON or request too large")
		return false
	}
	return true
}
func (p *Panel) platformCall(w http.ResponseWriter, r *http.Request, op map[string]any) {
	if p.cfg.Platforms == nil {
		writeErr(w, 503, "多平台功能未启用，请在配置中设置 platforms.enabled=true 后重启")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	raw, err := p.cfg.Platforms.Call(ctx, op, nil)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	writeJSON(w, 200, raw)
}
func (p *Panel) platformRPC(w http.ResponseWriter, r *http.Request) {
	var call struct {
		Method  string          `json:"method"`
		Payload json.RawMessage `json:"payload"`
	}
	if !platformPayload(w, r, &call) {
		return
	}
	// Only account and channel operations are exposed here.
	switch call.Method {
	case "account.list", "account.create", "account.import", "account.update", "account.delete", "account.refresh", "account.reorder", "account.reset",
		"login.poll", "login.sendSms", "login.submitSms", "credits.balances", "credits.status", "credits.permanentLock",
		"model.list", "model.setDisabled", "model.setAllDisabled", "provider.status", "provider.setEnabled",
		"backup.export", "backup.import", "onboarding.status", "onboarding.claim", "usage.tokenLedger", "usage.tokenLedgerHistory":
	default:
		writeErr(w, 400, "unsupported platform operation")
		return
	}
	if len(call.Payload) == 0 {
		call.Payload = json.RawMessage(`{}`)
	}
	p.platformCall(w, r, map[string]any{"type": "rpc", "call": call})
}
func (p *Panel) platformSchedule(w http.ResponseWriter, r *http.Request) {
	var payload map[string]any
	if !platformPayload(w, r, &payload) {
		return
	}
	p.platformCall(w, r, map[string]any{"type": "scheduleSet", "payload": payload})
}
func (p *Panel) platformCheckin(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Provider string `json:"provider"`
	}
	if !platformPayload(w, r, &payload) {
		return
	}
	switch payload.Provider {
	case "codearts", "lobsterai", "qoder", "qodercn", "trae", "loomy", "minimax":
	default:
		writeErr(w, http.StatusBadRequest, "该渠道不支持每日领取")
		return
	}
	p.platformCall(w, r, map[string]any{"type": "checkin", "provider": payload.Provider})
}
