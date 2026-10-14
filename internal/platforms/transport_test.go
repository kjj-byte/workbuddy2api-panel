package platforms

import (
	"io"
	"testing"
	"time"
)

func TestIdleStreamReleasesBlockedReader(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	body := newIdleBody(reader, 20*time.Millisecond)
	defer body.Close()
	result := make(chan error, 1)
	go func() { _, err := body.Read(make([]byte, 1)); result <- err }()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("idle stream closed without an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle stream remained blocked")
	}
}
