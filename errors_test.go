package iroh_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/discobox-ai/iroh-go"
)

// exchange drives a whole request/response on a fresh stream and reports what
// failed. A peer that goes away does so asynchronously, so which call notices
// depends on when the close lands; every one of them has to say the same
// thing about it.
func exchange(ctx context.Context, conn *iroh.Conn) error {
	send, recv, err := conn.OpenBi(ctx)
	if err != nil {
		return err
	}
	if _, err := send.Write([]byte("hello")); err != nil {
		return err
	}
	_, err = io.ReadAll(recv)
	return err
}

// A connection the peer closed is a connection-level failure: every stream on
// it is gone and the peer has to be redialled. Reporting it as a stream error
// left a caller unable to tell that from one stream ending.
func TestClosedConnectionIsAConnectionError(t *testing.T) {
	ctx := testContext(t)

	server := mustBind(t, ctx, localOptions(testALPN))
	client := mustBind(t, ctx, localOptions())

	serverAddr, err := server.Addr()
	if err != nil {
		t.Fatal(err)
	}

	const reason = "endpoint is not enrolled here"
	go func() {
		conn, err := server.Accept(ctx)
		if err != nil {
			return
		}
		_ = conn.CloseWithError(1, reason)
	}()

	conn, err := client.Connect(ctx, serverAddr, []byte(testALPN))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	err = exchange(ctx, conn)
	if err == nil {
		t.Fatal("exchange with a closed connection succeeded")
	}
	if !errors.Is(err, iroh.ErrConnection) {
		t.Errorf("error = %v, want ErrConnection", err)
	}
	if errors.Is(err, iroh.ErrStream) {
		t.Errorf("error = %v, want it not to look like a stream error", err)
	}
	// The peer's reason travels in the close, which is the only place a
	// refusal can explain itself.
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("error = %v, want it to carry the peer's reason", err)
	}
}

// The counterpart: a reset stream is a stream error, and leaves the
// connection it rode on usable.
func TestResetStreamIsAStreamError(t *testing.T) {
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
		send, _, err := conn.AcceptBi(ctx)
		if err != nil {
			return
		}
		_ = send.Reset(ctx, 7)
		// Hold the connection open, so what the client sees is the reset
		// stream rather than a connection that went away with this goroutine.
		<-ctx.Done()
	}()

	conn, err := client.Connect(ctx, serverAddr, []byte(testALPN))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	if err := exchange(ctx, conn); !errors.Is(err, iroh.ErrStream) {
		t.Fatalf("error = %v, want ErrStream", err)
	} else if errors.Is(err, iroh.ErrConnection) {
		t.Fatalf("error = %v, want it not to look like a connection error", err)
	}

	// The connection survived it.
	if _, _, err := conn.OpenBi(ctx); err != nil {
		t.Fatalf("connection unusable after a stream reset: %v", err)
	}
}
