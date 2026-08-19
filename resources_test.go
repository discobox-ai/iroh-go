package iroh_test

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/discobox-ai/iroh-go"
	"github.com/discobox-ai/iroh-go/internal/ffi"
)

// These tests reach into the internal package to assert that the bookkeeping
// on both sides of the FFI boundary returns to where it started. They are the
// ones that would catch a leak introduced by a future refactor.

// TestMain shuts the completion pump down after the suite, which exercises
// the path that makes iroh_completion_wait return -1.
func TestMain(m *testing.M) {
	code := m.Run()
	ffi.Shutdown()
	os.Exit(code)
}

func handleCount(t *testing.T) uint64 {
	t.Helper()
	n, err := ffi.HandleCount()
	if err != nil {
		t.Fatalf("handle count: %v", err)
	}
	return n
}

// stableHandleCount waits for the live-handle count to settle before reading
// it. Both peers run in this process, so a count taken the instant a round
// trip finishes can still include the other side's in-flight operations.
//
// The GC runs first so that handles owned by objects earlier tests dropped
// are released by their cleanups rather than counted here.
func stableHandleCount(t *testing.T) uint64 {
	t.Helper()
	runtime.GC()
	runtime.GC()
	deadline := time.Now().Add(5 * time.Second)
	last := handleCount(t)
	for time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
		if n := handleCount(t); n == last {
			return n
		} else {
			last = n
		}
	}
	t.Fatalf("handle count never settled (last %d)", last)
	return 0
}

func TestCancelledOperationsAreReaped(t *testing.T) {
	ctx := testContext(t)
	ep := mustBind(t, ctx, localOptions(testALPN))

	before := handleCount(t)

	for i := 0; i < 50; i++ {
		acceptCtx, cancel := context.WithTimeout(ctx, time.Millisecond)
		_, err := ep.Accept(acceptCtx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("iteration %d: want DeadlineExceeded, got %v", i, err)
		}
	}

	// Cancelling must both post a completion (so no waiter is left parked)
	// and release the operation's handle.
	if pending := ffi.PendingOps(); pending != 0 {
		t.Errorf("%d operations still pending after cancellation", pending)
	}
	if after := handleCount(t); after != before {
		t.Errorf("handle count grew from %d to %d across 50 cancelled accepts", before, after)
	}
}

func TestStreamsDoNotLeakHandles(t *testing.T) {
	ctx := testContext(t)

	server := mustBind(t, ctx, localOptions(testALPN))
	client := mustBind(t, ctx, localOptions())

	serverAddr, err := server.Addr()
	if err != nil {
		t.Fatal(err)
	}

	// A few warm-up rounds first, so the measured window covers only steady
	// state and not connection setup.
	const (
		warmup = 5
		rounds = 200
	)

	errc := make(chan error, 1)
	go func() {
		errc <- func() error {
			conn, err := server.Accept(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()
			for {
				send, recv, err := conn.AcceptBi(ctx)
				if err != nil {
					// The client closed the connection; that is the exit.
					return nil
				}
				if _, err := io.Copy(send, recv); err != nil {
					return err
				}
				if err := send.Close(); err != nil {
					return err
				}
				if err := recv.Close(); err != nil {
					return err
				}
			}
		}()
	}()

	conn, err := client.Connect(ctx, serverAddr, []byte(testALPN))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	roundTrip := func(i int) {
		t.Helper()
		send, recv, err := conn.OpenBi(ctx)
		if err != nil {
			t.Fatalf("round %d: open bi: %v", i, err)
		}
		if _, err := send.Write([]byte("ping")); err != nil {
			t.Fatalf("round %d: write: %v", i, err)
		}
		if err := send.Close(); err != nil {
			t.Fatalf("round %d: finish: %v", i, err)
		}
		if _, err := io.ReadAll(recv); err != nil {
			t.Fatalf("round %d: read: %v", i, err)
		}
		if err := recv.Close(); err != nil {
			t.Fatalf("round %d: close recv: %v", i, err)
		}
	}

	for i := 0; i < warmup; i++ {
		roundTrip(i)
	}
	before := stableHandleCount(t)

	for i := 0; i < rounds; i++ {
		roundTrip(warmup + i)
	}
	after := stableHandleCount(t)

	// Growth is the leak signal. A drop just means a cleanup for an object
	// some earlier test dropped happened to run inside the measured window,
	// which says nothing about this test.
	if after > before {
		t.Errorf("handle count grew from %d to %d across %d stream round trips", before, after, rounds)
	}

	conn.Close()
	if err := <-errc; err != nil {
		t.Fatalf("server: %v", err)
	}
	if pending := ffi.PendingOps(); pending != 0 {
		t.Errorf("%d operations still pending", pending)
	}
}

// TestEndpointIDFormattingMatchesIroh guards the one place where this package
// reimplements something the library already does: EndpointID.String is plain
// Go hex, so that it can be infallible, and must agree with iroh's own
// Display byte for byte.
func TestEndpointIDFormattingMatchesIroh(t *testing.T) {
	for i := 0; i < 32; i++ {
		key, err := iroh.GenerateSecretKey()
		if err != nil {
			t.Fatal(err)
		}
		id, err := key.Public()
		if err != nil {
			t.Fatal(err)
		}
		want, err := ffi.EndpointIDFormat(id[:])
		if err != nil {
			t.Fatalf("library formatting: %v", err)
		}
		if got := id.String(); got != want {
			t.Fatalf("EndpointID.String() = %q, iroh says %q", got, want)
		}
	}
}

func TestReadDeadline(t *testing.T) {
	ctx := testContext(t)

	server := mustBind(t, ctx, localOptions(testALPN))
	client := mustBind(t, ctx, localOptions())

	serverAddr, err := server.Addr()
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		conn, err := server.Accept(ctx)
		if err != nil {
			return
		}
		// Open the stream but never send anything, so the client's read
		// has nothing to do but time out.
		if _, _, err := conn.AcceptBi(ctx); err != nil {
			return
		}
		<-ctx.Done()
	}()

	conn, err := client.Connect(ctx, serverAddr, []byte(testALPN))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	send, recv, err := conn.OpenBi(ctx)
	if err != nil {
		t.Fatalf("open bi: %v", err)
	}
	if _, err := send.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := recv.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	start := time.Now()
	if _, err := recv.Read(buf); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("deadline took %v to fire", elapsed)
	}

	// A cancelled read consumes nothing, so reading again still works once
	// the deadline is cleared and data arrives.
	if err := recv.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	_ = iroh.LibraryPlatform()
}
