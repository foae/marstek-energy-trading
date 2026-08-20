package marstek

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLockContext_CancelsWhileWaiting(t *testing.T) {
	client := New("192.0.2.1:30000")
	client.mu.Lock()
	defer client.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := client.lockContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lockContext() error = %v, want context deadline exceeded", err)
	}
}

func TestSendContext_CancelledBeforeLock(t *testing.T) {
	client := New("192.0.2.1:30000")
	client.mu.Lock()
	defer client.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.sendContext(ctx, "ES.GetStatus", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sendContext() error = %v, want context canceled", err)
	}
}
