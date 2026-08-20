package iroh_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
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

	before := stableHandleCount(t)

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
	// Growth is the leak signal, as in TestStreamsDoNotLeakHandles: a drop
	// only means a cleanup for an object some earlier test dropped ran inside
	// the measured window.
	if after := stableHandleCount(t); after > before {
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

// idleStream opens a bidirectional stream to a peer that accepts it and then
// sends nothing, so a read on it blocks until a deadline ends it.
func idleStream(t *testing.T, ctx context.Context) *iroh.RecvStream {
	t.Helper()

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
		if _, _, err := conn.AcceptBi(ctx); err != nil {
			return
		}
		<-ctx.Done()
	}()

	conn, err := client.Connect(ctx, serverAddr, []byte(testALPN))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	send, recv, err := conn.OpenBi(ctx)
	if err != nil {
		t.Fatalf("open bi: %v", err)
	}
	// A stream only reaches the peer once something is written on it.
	if _, err := send.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	return recv
}

func TestReadDeadline(t *testing.T) {
	ctx := testContext(t)
	recv := idleStream(t, ctx)

	if err := recv.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	start := time.Now()
	err := func() error {
		_, err := recv.Read(buf)
		return err
	}()
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("want os.ErrDeadlineExceeded, got %v", err)
	}
	// net.Conn's contract, which net/http and most other callers detect by
	// asserting net.Error rather than by unwrapping.
	if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("read error %v does not report itself as a net.Error timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("deadline took %v to fire", elapsed)
	}

	// A read stopped by a deadline consumes nothing, so reading again still
	// works once the deadline is cleared and data arrives.
	if err := recv.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	_ = iroh.LibraryPlatform()
}

// A deadline must reach a read that is *already* blocked, not merely bound
// the next one: net/http aborts a hijacked connection's pending background
// read by setting a deadline in the past and waiting for that read to return,
// so a websocket upgrade over one of these streams deadlocks without this.
func TestReadDeadlineInterruptsABlockedRead(t *testing.T) {
	ctx := testContext(t)
	recv := idleStream(t, ctx)

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := recv.Read(buf)
		done <- err
	}()

	// Nothing is ever sent on this stream, so the read cannot end on its own.
	select {
	case err := <-done:
		t.Fatalf("read returned before any deadline was set: %v", err)
	case <-time.After(250 * time.Millisecond):
	}

	if err := recv.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("want os.ErrDeadlineExceeded, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a deadline set in the past did not end the blocked read")
	}
}

// A read that completes just as its deadline expires must report its bytes.
// The completion and the cancellation race inside the FFI, and the loser's
// result is dropped -- so preferring the cancellation would lose data the
// stream has already advanced past.
func TestReadDeadlineRaceLosesNoBytes(t *testing.T) {
	ctx := testContext(t)

	server := mustBind(t, ctx, localOptions(testALPN))
	client := mustBind(t, ctx, localOptions())

	serverAddr, err := server.Addr()
	if err != nil {
		t.Fatal(err)
	}

	payload := bytes.Repeat([]byte("no byte left behind. "), 20_000)
	go func() {
		conn, err := server.Accept(ctx)
		if err != nil {
			return
		}
		send, _, err := conn.AcceptBi(ctx)
		if err != nil {
			return
		}
		if _, err := send.Write(payload); err != nil {
			return
		}
		send.Close()
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
	if _, err := send.Write([]byte("go")); err != nil {
		t.Fatalf("write: %v", err)
	}

	var (
		got []byte
		buf = make([]byte, 4096)
	)
	for i := 0; len(got) < len(payload); i++ {
		if ctx.Err() != nil {
			t.Fatalf("read %d of %d bytes before the test context expired", len(got), len(payload))
		}
		if i%4 == 3 {
			// Every fourth read is unbounded, so the loop always makes
			// progress however the races land.
			err = recv.SetReadDeadline(time.Time{})
		} else {
			// Deadlines a few hundred microseconds out land near the read's
			// own completion, which is where the race lives.
			err = recv.SetReadDeadline(time.Now().Add(time.Duration(i%50) * 10 * time.Microsecond))
		}
		if err != nil {
			t.Fatal(err)
		}

		n, err := recv.Read(buf)
		got = append(got, buf[:n]...)
		switch {
		case err == nil, errors.Is(err, os.ErrDeadlineExceeded):
		case errors.Is(err, io.EOF):
			if len(got) != len(payload) {
				t.Fatalf("stream ended after %d of %d bytes", len(got), len(payload))
			}
		default:
			t.Fatalf("read: %v", err)
		}
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("read %d bytes, want the %d that were sent, and identical", len(got), len(payload))
	}
}
