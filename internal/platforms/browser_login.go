package platforms

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var errCallbackParameters = errors.New("登录回调参数不匹配，请从面板重新打开登录页面")

// A fresh loopback listener belongs to exactly one login. Credentials never
// travel through the panel: only the completed account ID is returned there.
func browserCallback(ctx context.Context, path string, origins []string, buildURL func(string) string, exchange func(context.Context, *http.Request) (Credential, error)) (BrowserAuthorization, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return BrowserAuthorization{}, fmt.Errorf("无法启动本地登录回调: %w", err)
	}
	address := listener.Addr().String()
	result := make(chan LoginResult, 1)
	done := make(chan struct{})
	var once sync.Once
	var busy atomic.Bool
	finish := func(c Credential, err error) { once.Do(func() { result <- LoginResult{c, err}; close(done) }) }
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 150 * time.Second, MaxHeaderBytes: 64 << 10}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.Host != address || r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			allowed := false
			for _, candidate := range origins {
				if origin == candidate {
					allowed = true
					break
				}
			}
			if !allowed {
				http.Error(w, "登录来源无效", 403)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		if !busy.CompareAndSwap(false, true) {
			http.Error(w, "登录回调已在处理", 409)
			return
		}
		credential, err := exchange(ctx, r)
		if errors.Is(err, errCallbackParameters) {
			busy.Store(false)
			http.Error(w, err.Error(), 400)
			return
		}
		if err == nil {
			_, err = fmt.Fprintln(w, "登录成功，可以关闭此页面并返回网关。")
			finish(credential, nil)
		} else {
			http.Error(w, "登录未完成，请返回面板查看原因。", 502)
			finish(nil, err)
		}
	})
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			finish(nil, fmt.Errorf("登录回调服务已停止"))
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			finish(nil, ctx.Err())
		case <-done:
		}
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		_ = server.Close()
	}()
	return BrowserAuthorization{URL: buildURL("http://" + address + path), Result: result}, nil
}

func scalarID(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if n, ok := number(v); ok {
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return ""
}
func jsonParam(raw string) map[string]any {
	for i := 0; i < 3; i++ {
		var data map[string]any
		if json.Unmarshal([]byte(raw), &data) == nil && data != nil {
			return data
		}
		next, err := url.QueryUnescape(raw)
		if err != nil || next == raw {
			break
		}
		raw = next
	}
	return map[string]any{}
}
func jwtExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return 0
	}
	seconds, ok := number(claims["exp"])
	if !ok || seconds <= 0 {
		return 0
	}
	return int64(seconds * 1000)
}
