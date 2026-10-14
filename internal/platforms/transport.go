package platforms

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// Keep streaming requests unbounded in total duration, but release connections
// that stop producing bytes. Cancellation still propagates through net/http.
type idleTransport struct {
	base    http.RoundTripper
	timeout time.Duration
}

func (t idleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = newIdleBody(resp.Body, t.timeout)
	return resp, nil
}

type idleBody struct {
	io.ReadCloser
	mu      sync.Mutex
	timer   *time.Timer
	timeout time.Duration
	closed  bool
}

func newIdleBody(body io.ReadCloser, timeout time.Duration) *idleBody {
	b := &idleBody{ReadCloser: body, timeout: timeout}
	b.timer = time.AfterFunc(timeout, func() { _ = b.Close() })
	return b
}
func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	if !b.closed {
		b.timer.Reset(b.timeout)
	}
	b.mu.Unlock()
	return n, err
}
func (b *idleBody) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.timer.Stop()
	b.mu.Unlock()
	return b.ReadCloser.Close()
}
