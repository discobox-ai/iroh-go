package iroh_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/netip"
	"testing"
	"time"

	"github.com/discobox-ai/iroh-go"
)

const testALPN = "iroh-go/test/1"

// localOptions configures an endpoint that talks only over loopback: no
// relays, no address lookup, no network access. Everything in this file runs
// offline and deterministically.
func localOptions(alpns ...string) iroh.Options {
	opts := iroh.Options{
		Preset:    iroh.PresetMinimal,
		RelayMode: iroh.RelayDisabled,
		BindAddrs: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:0")},
	}
	for _, a := range alpns {
		opts.ALPNs = append(opts.ALPNs, []byte(a))
	}
	return opts
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mustBind(t *testing.T, ctx context.Context, opts iroh.Options) *iroh.Endpoint {
	t.Helper()
	ep, err := iroh.Bind(ctx, opts)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ep.Close(closeCtx)
	})
	return ep
}

func TestSecretKeyAndEndpointID(t *testing.T) {
	key, err := iroh.GenerateSecretKey()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	id, err := key.Public()
	if err != nil {
		t.Fatalf("public: %v", err)
	}
	if id.IsZero() {
		t.Fatal("derived endpoint id is zero")
	}

	// The Go-side hex formatting must agree with what iroh's own parser
	// accepts, which is the whole reason String is implemented in Go.
	parsed, err := iroh.ParseEndpointID(id.String())
	if err != nil {
		t.Fatalf("parse %q: %v", id.String(), err)
	}
	if parsed != id {
		t.Fatalf("round trip changed the id: %v -> %v", id, parsed)
	}

	msg := []byte("attack at dawn")
	sig, err := key.Sign(msg)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := id.Verify(msg, sig); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := id.Verify([]byte("attack at dusk"), sig); err == nil {
		t.Fatal("verify accepted a signature over a different message")
	} else if !errors.Is(err, iroh.ErrKeyParsing) {
		t.Fatalf("want ErrKeyParsing, got %v", err)
	}
}

func TestEndpointIDRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "not-hex", "00"} {
		if _, err := iroh.ParseEndpointID(s); err == nil {
			t.Errorf("ParseEndpointID(%q) succeeded, want an error", s)
		}
	}
}

func TestTicketRoundTrip(t *testing.T) {
	key, err := iroh.GenerateSecretKey()
	if err != nil {
		t.Fatal(err)
	}
	id, err := key.Public()
	if err != nil {
		t.Fatal(err)
	}

	addr := iroh.AddrOf(id).
		WithRelayURL("https://relay.example./").
		WithDirectAddrs(
			netip.MustParseAddrPort("192.0.2.1:4433"),
			netip.MustParseAddrPort("[2001:db8::1]:4433"),
		)

	ticket, err := addr.Ticket()
	if err != nil {
		t.Fatalf("ticket: %v", err)
	}
	if ticket == "" {
		t.Fatal("empty ticket")
	}

	back, err := ticket.Addr()
	if err != nil {
		t.Fatalf("decode ticket: %v", err)
	}
	if back.ID != addr.ID {
		t.Errorf("id: got %v, want %v", back.ID, addr.ID)
	}
	if back.RelayURL != addr.RelayURL {
		t.Errorf("relay: got %q, want %q", back.RelayURL, addr.RelayURL)
	}
	if len(back.DirectAddrs) != len(addr.DirectAddrs) {
		t.Fatalf("direct addrs: got %v, want %v", back.DirectAddrs, addr.DirectAddrs)
	}

	if _, err := iroh.ParseTicket("definitely-not-a-ticket"); !errors.Is(err, iroh.ErrTicketParsing) {
		t.Errorf("want ErrTicketParsing, got %v", err)
	}
}

func TestBidirectionalStreamEcho(t *testing.T) {
	ctx := testContext(t)

	server := mustBind(t, ctx, localOptions(testALPN))
	client := mustBind(t, ctx, localOptions())

	serverAddr, err := server.Addr()
	if err != nil {
		t.Fatalf("server addr: %v", err)
	}
	if len(serverAddr.DirectAddrs) == 0 {
		t.Fatalf("server has no direct addresses: %v", serverAddr)
	}

	// Echo everything the client sends, on the same stream.
	errc := make(chan error, 1)
	go func() {
		errc <- func() error {
			conn, err := server.Accept(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			send, recv, err := conn.AcceptBi(ctx)
			if err != nil {
				return err
			}
			if _, err := io.Copy(send, recv); err != nil {
				return err
			}
			return send.Close()
		}()
	}()

	conn, err := client.Connect(ctx, serverAddr, []byte(testALPN))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	remote, err := conn.RemoteID()
	if err != nil {
		t.Fatalf("remote id: %v", err)
	}
	if remote != server.ID() {
		t.Errorf("remote id: got %v, want %v", remote, server.ID())
	}
	if alpn, err := conn.ALPN(); err != nil {
		t.Errorf("alpn: %v", err)
	} else if string(alpn) != testALPN {
		t.Errorf("alpn: got %q, want %q", alpn, testALPN)
	}

	send, recv, err := conn.OpenBi(ctx)
	if err != nil {
		t.Fatalf("open bi: %v", err)
	}

	// Large enough to span several flow-control windows, so the write loop
	// and the chunked read path both get exercised.
	payload := bytes.Repeat([]byte("iroh-go without cgo. "), 50_000)
	go func() {
		if _, err := send.Write(payload); err != nil {
			t.Errorf("write: %v", err)
		}
		send.Close()
	}()

	got, err := io.ReadAll(recv)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatch: got %d bytes, want %d", len(got), len(payload))
	}

	if err := <-errc; err != nil {
		t.Fatalf("server: %v", err)
	}

	if stats, err := conn.Stats(); err != nil {
		t.Errorf("stats: %v", err)
	} else if stats.SentBytes == 0 || stats.ReceivedBytes == 0 {
		t.Errorf("stats look empty: %+v", stats)
	}
}

func TestUnidirectionalStreamAndDatagram(t *testing.T) {
	ctx := testContext(t)

	server := mustBind(t, ctx, localOptions(testALPN))
	client := mustBind(t, ctx, localOptions())

	serverAddr, err := server.Addr()
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		stream   []byte
		datagram []byte
		err      error
	}
	results := make(chan result, 1)
	go func() {
		var r result
		r.err = func() error {
			conn, err := server.Accept(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			recv, err := conn.AcceptUni(ctx)
			if err != nil {
				return err
			}
			if r.stream, err = io.ReadAll(recv); err != nil {
				return err
			}
			r.datagram, err = conn.ReadDatagram(ctx)
			return err
		}()
		results <- r
	}()

	conn, err := client.Connect(ctx, serverAddr, []byte(testALPN))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	send, err := conn.OpenUni(ctx)
	if err != nil {
		t.Fatalf("open uni: %v", err)
	}
	if _, err := send.Write([]byte("one way")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := send.Close(); err != nil {
		t.Fatalf("finish: %v", err)
	}

	if max, err := conn.MaxDatagramSize(); err != nil {
		t.Errorf("max datagram size: %v", err)
	} else if max == 0 {
		t.Error("datagrams unsupported on a loopback connection")
	}
	if err := conn.SendDatagram([]byte("unreliable")); err != nil {
		t.Fatalf("send datagram: %v", err)
	}

	r := <-results
	if r.err != nil {
		t.Fatalf("server: %v", r.err)
	}
	if string(r.stream) != "one way" {
		t.Errorf("uni stream: got %q", r.stream)
	}
	if string(r.datagram) != "unreliable" {
		t.Errorf("datagram: got %q", r.datagram)
	}
}

func TestAcceptRespectsContextCancellation(t *testing.T) {
	ctx := testContext(t)
	ep := mustBind(t, ctx, localOptions(testALPN))

	acceptCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := ep.Accept(acceptCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cancellation took %v", elapsed)
	}
}

func TestClosedEndpointReportsErrClosed(t *testing.T) {
	ctx := testContext(t)
	ep, err := iroh.Bind(ctx, localOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := ep.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Close is idempotent.
	if err := ep.Close(ctx); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := ep.Addr(); !errors.Is(err, iroh.ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}
