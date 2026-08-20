package iroh_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/iroh-go"
)

// Stress tests: concurrency, volume and churn, which the functional tests
// cannot cover because they do one thing at a time. What they are looking for
// is a lost byte, a leaked handle or goroutine, and a call that never returns
// -- the three ways this layer fails that a single-threaded test still passes.
//
// They run offline on loopback with both peers in this process, so they are
// deterministic enough for CI, and skip under -short.

// echoServer serves every stream a peer opens by echoing it back, and reports
// how many it served.
func echoServer(t *testing.T, ctx context.Context, ep *iroh.Endpoint) (*iroh.Listener, *atomic.Int64) {
	t.Helper()
	listener := ep.Listener(iroh.ListenOptions{})
	t.Cleanup(func() { _ = listener.Close() })

	var served atomic.Int64
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if _, err := io.Copy(c, c); err != nil {
					return
				}
				// Half-close so the client's ReadAll terminates.
				if cw, ok := c.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
				served.Add(1)
			}(conn)
		}
	}()
	return listener, &served
}

func stressPair(t *testing.T, ctx context.Context) (*iroh.Conn, *atomic.Int64) {
	t.Helper()
	server := mustBind(t, ctx, localOptions(testALPN))
	client := mustBind(t, ctx, localOptions())

	addr, err := server.Addr()
	if err != nil {
		t.Fatal(err)
	}
	_, served := echoServer(t, ctx, server)

	conn, err := client.Connect(ctx, addr, []byte(testALPN))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, served
}

// echoOnce opens a stream, sends payload, and requires the same bytes back.
func echoOnce(conn *iroh.Conn, ctx context.Context, payload []byte) error {
	stream, err := conn.OpenConn(ctx)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer stream.Close()

	writeErr := make(chan error, 1)
	go func() {
		_, err := stream.Write(payload)
		if err == nil {
			err = stream.CloseWrite()
		}
		writeErr <- err
	}()

	got, err := io.ReadAll(stream)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if err := <-writeErr; err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if !bytes.Equal(got, payload) {
		return fmt.Errorf("echo mismatch: got %d bytes, sent %d", len(got), len(payload))
	}
	return nil
}

// Many streams at once on one connection, with payloads spanning several flow
// control windows. A lost or duplicated byte anywhere shows up as a mismatch,
// and the handle count says whether the streams were reclaimed.
func TestStressConcurrentStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("stress")
	}
	ctx := testContext(t)
	conn, served := stressPair(t, ctx)

	const (
		workers        = 16
		streamsPerWork = 25
	)
	before := stableHandleCount(t)

	var wg sync.WaitGroup
	errs := make(chan error, workers*streamsPerWork)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for i := 0; i < streamsPerWork; i++ {
				payload := make([]byte, 1+rng.Intn(256*1024))
				if _, err := rng.Read(payload); err != nil {
					errs <- err
					return
				}
				if err := echoOnce(conn, ctx, payload); err != nil {
					errs <- err
					return
				}
			}
		}(int64(w))
	}
	wg.Wait()
	close(errs)

	failed := 0
	for err := range errs {
		failed++
		if failed <= 5 {
			t.Errorf("stream: %v", err)
		}
	}
	if failed > 5 {
		t.Errorf("... and %d more", failed-5)
	}

	if got := served.Load(); got != workers*streamsPerWork {
		t.Errorf("server echoed %d streams, want %d", got, workers*streamsPerWork)
	}
	if after := stableHandleCount(t); after > before {
		t.Errorf("handle count grew from %d to %d across %d streams", before, after, workers*streamsPerWork)
	}
}

// The same volume, but with a deadline moving under every read. This is the
// shape that lost bytes before: the read completes and the deadline expires
// at the same instant, and whichever one is reported has to be the one that
// actually happened.
func TestStressDeadlineChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("stress")
	}
	ctx := testContext(t)
	conn, _ := stressPair(t, ctx)

	const streams = 40
	var wg sync.WaitGroup
	errs := make(chan error, streams)
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			payload := bytes.Repeat([]byte(fmt.Sprintf("stream-%02d.", n)), 8*1024)

			stream, err := conn.OpenConn(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer stream.Close()

			go func() {
				_, _ = stream.Write(payload)
				_ = stream.CloseWrite()
			}()

			var (
				got []byte
				buf = make([]byte, 4096)
			)
			for len(got) < len(payload) {
				if ctx.Err() != nil {
					errs <- fmt.Errorf("stream %d stalled at %d of %d bytes", n, len(got), len(payload))
					return
				}
				// Deadlines land near the read's own completion, which is
				// where the race lives. Every fourth read is unbounded so the
				// loop always makes progress.
				if n%4 == 3 {
					err = stream.SetReadDeadline(time.Time{})
				} else {
					err = stream.SetReadDeadline(time.Now().Add(time.Duration(n%25) * 20 * time.Microsecond))
				}
				if err != nil {
					errs <- err
					return
				}
				read, err := stream.Read(buf)
				got = append(got, buf[:read]...)
				switch {
				case err == nil, errors.Is(err, os.ErrDeadlineExceeded):
				case errors.Is(err, io.EOF):
					if len(got) != len(payload) {
						errs <- fmt.Errorf("stream %d ended at %d of %d bytes", n, len(got), len(payload))
						return
					}
				default:
					errs <- fmt.Errorf("stream %d read: %w", n, err)
					return
				}
			}
			if !bytes.Equal(got, payload) {
				errs <- fmt.Errorf("stream %d echoed different bytes", n)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Closing a stream while it is being read and written must end those calls
// rather than hang, and must not leave anything behind. A deadlock here would
// hold the whole suite until its timeout, which is the point: that is what
// this class of bug looks like.
func TestStressCloseDuringIO(t *testing.T) {
	if testing.Short() {
		t.Skip("stress")
	}
	ctx := testContext(t)
	conn, _ := stressPair(t, ctx)

	const rounds = 60
	payload := bytes.Repeat([]byte("close me. "), 4096)

	for i := 0; i < rounds; i++ {
		stream, err := conn.OpenConn(ctx)
		if err != nil {
			t.Fatalf("round %d open: %v", i, err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for {
				if _, err := stream.Write(payload); err != nil {
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			buf := make([]byte, 8192)
			for {
				if _, err := stream.Read(buf); err != nil {
					return
				}
			}
		}()

		// Close from a third goroutine while both are in flight.
		time.Sleep(time.Duration(i%5) * time.Millisecond)
		closed := make(chan error, 1)
		go func() { closed <- stream.Close() }()

		select {
		case err := <-closed:
			if err != nil {
				t.Fatalf("round %d close: %v", i, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: Close blocked behind its own stream", i)
		}

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: a read or write outlived the close", i)
		}
	}
}

// Connections, listeners and endpoints churned end to end. Anything this
// layer forgets to release shows up as a goroutine or a handle that never
// comes back.
func TestStressConnectionChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("stress")
	}
	ctx := testContext(t)

	settle := func() int {
		for i := 0; i < 100; i++ {
			runtime.GC()
			time.Sleep(20 * time.Millisecond)
			n := runtime.NumGoroutine()
			runtime.GC()
			time.Sleep(20 * time.Millisecond)
			if runtime.NumGoroutine() == n {
				return n
			}
		}
		return runtime.NumGoroutine()
	}

	// One warm-up cycle first, so one-time setup is not counted as a leak.
	churn := func(round int) {
		server := mustBind(t, ctx, localOptions(testALPN))
		client := mustBind(t, ctx, localOptions())
		addr, err := server.Addr()
		if err != nil {
			t.Fatalf("round %d addr: %v", round, err)
		}
		listener := server.Listener(iroh.ListenOptions{})
		go func() {
			for {
				c, err := listener.Accept()
				if err != nil {
					return
				}
				go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
			}
		}()

		conn, err := client.Connect(ctx, addr, []byte(testALPN))
		if err != nil {
			t.Fatalf("round %d connect: %v", round, err)
		}
		for i := 0; i < 3; i++ {
			if err := echoOnce(conn, ctx, []byte(fmt.Sprintf("round %d stream %d", round, i))); err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		_ = conn.Close()
		_ = listener.Close()

		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Close(closeCtx)
		_ = server.Close(closeCtx)
	}

	churn(0)
	baseGoroutines := settle()
	baseHandles := stableHandleCount(t)

	for round := 1; round <= 10; round++ {
		churn(round)
	}

	if got := settle(); got > baseGoroutines+2 {
		t.Errorf("goroutines grew from %d to %d across 10 endpoint lifecycles", baseGoroutines, got)
	}
	if got := stableHandleCount(t); got > baseHandles {
		t.Errorf("handle count grew from %d to %d across 10 endpoint lifecycles", baseHandles, got)
	}
}

// A net.Conn is a stream and nothing else: the caller it is handed to holds no
// connection and no endpoint, because those are not part of that contract. So
// a stream has to keep whatever it needs alive by itself. If it does not, the
// collector takes the connection out from under a caller that did nothing
// wrong, and the failure lands as "connection lost: closed" in the middle of a
// working exchange.
//
// The collector runs throughout, because that is the only thing that
// distinguishes this from any other echo.
func TestStressStreamSurvivesCollection(t *testing.T) {
	if testing.Short() {
		t.Skip("stress")
	}
	ctx := testContext(t)

	server := mustBind(t, ctx, localOptions(testALPN))
	addr, err := server.Addr()
	if err != nil {
		t.Fatal(err)
	}
	listener := server.Listener(iroh.ListenOptions{})
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 64)
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				// Answer slowly, so the client is mid-exchange rather than
				// racing to finish before the collector runs.
				time.Sleep(300 * time.Millisecond)
				_, _ = c.Write(buf[:n])
			}(c)
		}
	}()

	// Everything except the stream goes out of scope here, which is what
	// handing a net.Conn to a caller looks like.
	var stream net.Conn
	func() {
		client := mustBind(t, ctx, localOptions())
		conn, err := client.Connect(ctx, addr, []byte(testALPN))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		stream, err = conn.OpenConn(ctx)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
	}()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				runtime.GC()
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	want := []byte("still here")
	if _, err := stream.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := stream.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("echo = %q, want %q", got, want)
	}
}

// streamToNowhere returns a stream whose endpoint and connection are
// unreachable the moment it returns. That is not an exotic situation: it is
// what handing a net.Conn to a caller looks like, since net.Conn names neither
// of them. mustBind is deliberately not used, because the cleanup it registers
// would hold the endpoint alive for the whole test and hide the thing being
// tested.
func streamToNowhere(t *testing.T, ctx context.Context, addr iroh.EndpointAddr) net.Conn {
	t.Helper()
	client, err := iroh.Bind(ctx, localOptions())
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	conn, err := client.Connect(ctx, addr, []byte(testALPN))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	stream, err := conn.OpenConn(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return stream
}

// A stream has to keep alive whatever it needs to work. Its connection dies
// with the endpoint, and the endpoint is freed when Go collects it, so a
// stream that does not hold its own chain is one collection away from
// "connection lost: closed" in the middle of a working exchange -- through no
// fault of the caller, who is holding exactly what the interface gave them.
func TestStressStreamKeepsItsEndpointAlive(t *testing.T) {
	if testing.Short() {
		t.Skip("stress")
	}
	ctx := testContext(t)

	server := mustBind(t, ctx, localOptions(testALPN))
	addr, err := server.Addr()
	if err != nil {
		t.Fatal(err)
	}
	listener := server.Listener(iroh.ListenOptions{})
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 64)
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				// Slowly, so the exchange is still in flight while the
				// collector runs rather than finishing before it.
				time.Sleep(200 * time.Millisecond)
				_, _ = c.Write(buf[:n])
			}(c)
		}
	}()

	for round := 0; round < 5; round++ {
		stream := streamToNowhere(t, ctx, addr)

		stop := make(chan struct{})
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
					runtime.GC()
					time.Sleep(time.Millisecond)
				}
			}
		}()

		want := []byte(fmt.Sprintf("round %d", round))
		err := func() error {
			defer close(stop)
			defer stream.Close()
			if _, err := stream.Write(want); err != nil {
				return fmt.Errorf("write: %w", err)
			}
			if err := stream.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
				return err
			}
			got := make([]byte, len(want))
			if _, err := io.ReadFull(stream, got); err != nil {
				return fmt.Errorf("read: %w", err)
			}
			if !bytes.Equal(got, want) {
				return fmt.Errorf("echo = %q, want %q", got, want)
			}
			return nil
		}()
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
}
