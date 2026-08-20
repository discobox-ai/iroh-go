package iroh

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

// These exercise the deadline bookkeeping directly, without a native library,
// so the net.Conn semantics they encode are checked even where the cdylib for
// this platform is missing.

func TestDeadlineReachesAnArmedCall(t *testing.T) {
	var d deadline
	ctx, release := d.arm()
	defer release()

	select {
	case <-ctx.Done():
		t.Fatal("armed context is done before any deadline was set")
	default:
	}

	d.set(time.Now().Add(-time.Second))

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("setting a deadline did not reach the armed context")
	}
	// Cancelled, not expired: the call this woke re-derives its context from
	// the new deadline rather than reporting a timeout that may not have
	// happened.
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("armed context ended with %v, want context.Canceled", ctx.Err())
	}
}

func TestDeadlineRearmSeesTheNewDeadline(t *testing.T) {
	var d deadline
	d.set(time.Now().Add(-time.Second))

	ctx, release := d.arm()
	defer release()

	if err := ctx.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("arming under a deadline in the past gave %v, want DeadlineExceeded", err)
	}
}

func TestDeadlineClearedIsUnbounded(t *testing.T) {
	var d deadline
	d.set(time.Now().Add(-time.Second))
	d.set(time.Time{})

	ctx, release := d.arm()
	defer release()

	select {
	case <-ctx.Done():
		t.Fatalf("cleared deadline still bounds the call: %v", ctx.Err())
	default:
	}
}

func TestDeadlineReachesEveryArmedCall(t *testing.T) {
	var d deadline
	read, releaseRead := d.arm()
	defer releaseRead()
	write, releaseWrite := d.arm()
	defer releaseWrite()

	d.set(time.Now())

	for name, ctx := range map[string]context.Context{"read": read, "write": write} {
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Errorf("%s context was not reached by the deadline", name)
		}
	}
}

func TestDeadlineReleaseUnregisters(t *testing.T) {
	var d deadline
	_, release := d.arm()
	release()

	d.mu.Lock()
	armed := len(d.armed)
	d.mu.Unlock()
	if armed != 0 {
		t.Errorf("%d contexts still registered after release", armed)
	}

	// A finished call must not be cancelled by a later deadline, and set must
	// not trip over the registration it removed.
	d.set(time.Now())
}

func TestDeadlineErr(t *testing.T) {
	err := deadlineErr(context.DeadlineExceeded)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("deadlineErr(DeadlineExceeded) = %v, want os.ErrDeadlineExceeded", err)
	}
	// Bare, not wrapped: net/http and friends detect a timeout by asserting
	// net.Error, which wrapping defeats.
	if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("%v does not report itself as a net.Error timeout", err)
	}

	other := errors.New("boom")
	if got := deadlineErr(other); !errors.Is(got, other) {
		t.Fatalf("deadlineErr rewrote an unrelated error: %v", got)
	}
}
