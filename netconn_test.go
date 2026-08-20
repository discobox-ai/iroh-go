package iroh_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/iroh-go"
)

// connectedPair binds a server and a client endpoint on loopback and dials
// one from the other, returning the listener and the client's connection to
// it.
func connectedPair(t *testing.T, ctx context.Context, opts iroh.ListenOptions) (*iroh.Listener, *iroh.Endpoint, *iroh.Conn) {
	t.Helper()

	server := mustBind(t, ctx, localOptions(testALPN))
	client := mustBind(t, ctx, localOptions())

	serverAddr, err := server.Addr()
	if err != nil {
		t.Fatalf("server addr: %v", err)
	}

	listener := server.Listener(opts)
	t.Cleanup(func() { _ = listener.Close() })

	conn, err := client.Connect(ctx, serverAddr, []byte(testALPN))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return listener, client, conn
}

// The point of the net.Listener shape is that a server written against it
// serves iroh unchanged, so the test drives a real http.Server rather than a
// stand-in.
func TestListenerServesHTTP(t *testing.T) {
	ctx := testContext(t)
	listener, client, conn := connectedPair(t, ctx, iroh.ListenOptions{})

	mux := http.NewServeMux()
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		// The peer's identity arrives with the connection, so a server can
		// build a principal from it without asking the client who it is.
		_, _ = io.WriteString(w, r.RemoteAddr)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	httpClient := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return conn.OpenConn(ctx)
		},
	}}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://iroh.invalid/whoami", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := string(body); got != client.ID().String() {
		t.Fatalf("server saw peer %q, want %q", got, client.ID())
	}

	if got := listener.Addr().String(); got != listener.Addr().(iroh.Addr).ID.String() {
		t.Fatalf("listener addr %q does not render its endpoint id", got)
	}
}

// A refused peer has to be told why. The reason travels in the connection
// close, which is the only place a refusal can explain itself.
func TestListenerRefusesUnauthorizedPeer(t *testing.T) {
	ctx := testContext(t)
	const reason = "endpoint is not enrolled here"
	_, _, conn := connectedPair(t, ctx, iroh.ListenOptions{
		Authorize: func(*iroh.Conn) error { return errors.New(reason) },
	})

	// The refusal lands on the stream open or on the exchange that follows,
	// depending on whether the close has arrived yet, so drive the whole
	// thing and require that whatever fails says why.
	err := func() error {
		stream, err := conn.OpenConn(ctx)
		if err != nil {
			return err
		}
		defer stream.Close()
		if _, err := stream.Write([]byte("hello")); err != nil {
			return err
		}
		_, err = io.ReadAll(stream)
		return err
	}()
	if err == nil {
		t.Fatal("a refused peer completed an exchange")
	}
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("error = %v, want it to carry the refusal reason", err)
	}
	// Connection-level, not stream-level: every stream on this connection is
	// gone and the peer has to be redialled.
	if !errors.Is(err, iroh.ErrConnection) {
		t.Errorf("error = %v, want ErrConnection", err)
	}
}

// CloseWrite is what a protocol that says "that is all I have to send" needs,
// and what net/http and crypto/ssh reach for through the same interface
// assertion *net.TCPConn satisfies.
func TestStreamConnHalfClose(t *testing.T) {
	ctx := testContext(t)
	listener, _, conn := connectedPair(t, ctx, iroh.ListenOptions{})

	served := make(chan error, 1)
	go func() {
		served <- func() error {
			accepted, err := listener.Accept()
			if err != nil {
				return err
			}
			defer accepted.Close()
			// Reading to EOF only terminates because the peer half-closed.
			got, err := io.ReadAll(accepted)
			if err != nil {
				return err
			}
			if !bytes.Equal(got, []byte("ping")) {
				t.Errorf("server read %q, want %q", got, "ping")
			}
			_, err = accepted.Write([]byte("pong"))
			return err
		}()
	}()

	stream, err := conn.OpenConn(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer stream.Close()

	if _, err := stream.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := stream.CloseWrite(); err != nil {
		t.Fatalf("close write: %v", err)
	}
	// The read half is still open, which is the whole point.
	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, []byte("pong")) {
		t.Fatalf("client read %q, want %q", got, "pong")
	}
	if err := <-served; err != nil {
		t.Fatalf("server: %v", err)
	}
}

// Close has to interrupt whatever is holding the stream, and report itself as
// a close rather than as the timeout it used to get there.
func TestStreamConnCloseUnblocksARead(t *testing.T) {
	ctx := testContext(t)
	listener, _, conn := connectedPair(t, ctx, iroh.ListenOptions{})

	go func() {
		// Accept the stream and send nothing, so the client's read has
		// nothing to do but wait.
		if accepted, err := listener.Accept(); err == nil {
			<-ctx.Done()
			_ = accepted.Close()
		}
	}()

	stream, err := conn.OpenConn(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := stream.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}

	read := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := stream.Read(buf)
		read <- err
	}()

	select {
	case err := <-read:
		t.Fatalf("read returned before the close: %v", err)
	case <-time.After(250 * time.Millisecond):
	}

	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case err := <-read:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("read after Close = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock the pending read")
	}
}

func TestListenerAcceptReportsClosed(t *testing.T) {
	ctx := testContext(t)
	ep := mustBind(t, ctx, localOptions(testALPN))

	listener := ep.Listener(iroh.ListenOptions{})
	if err := listener.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after Close = %v, want net.ErrClosed", err)
	}
	// Closing the listener must not close the endpoint under it.
	if _, err := ep.Addr(); err != nil {
		t.Fatalf("endpoint unusable after its listener closed: %v", err)
	}
}
