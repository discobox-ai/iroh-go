package iroh

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// This file is what makes iroh usable with the rest of Go. A QUIC stream is
// already an ordered reliable byte stream between two authenticated parties,
// which is what net.Conn describes, so anything written against net.Conn and
// net.Listener -- net/http, gRPC, an SSH server, a reverse proxy -- runs over
// iroh unchanged once the shapes line up.
//
// The shapes are small but each has one trap in it, and every caller that
// writes this file for themselves gets to find them: a deadline has to reach
// a call that is already blocked, a Close has to interrupt whatever is
// holding the stream, and a read after Close has to report net.ErrClosed
// rather than the timeout used to unblock it.

// Addr is the net.Addr of an iroh endpoint. The identity is the address:
// there is nothing to resolve and nothing else to trust.
type Addr struct {
	ID EndpointID
}

// Network returns "iroh".
func (Addr) Network() string { return "iroh" }

// String is the endpoint id in the hex form iroh renders, or "" if unset.
func (a Addr) String() string {
	if a.ID.IsZero() {
		return ""
	}
	return a.ID.String()
}

// refusedCode is the QUIC application error code a [Listener] closes an
// unauthorized connection with. The reason string is where the explanation
// is; the code only distinguishes "we refused you" from a clean close.
const refusedCode = 1

// StreamConn presents one bidirectional stream as a [net.Conn].
//
// Streams are cheap, so the usual shape is one stream per request or per
// session rather than one per peer: the connection underneath carries the
// handshake, the path discovery and the NAT traversal state, and it is worth
// keeping.
type StreamConn struct {
	send   *SendStream
	recv   *RecvStream
	local  EndpointID
	remote EndpointID

	closeOnce sync.Once
	closeErr  error
	closed    chan struct{}
}

var (
	_ net.Conn     = (*StreamConn)(nil)
	_ net.Listener = (*Listener)(nil)
)

// OpenConn opens a bidirectional stream and presents it as a [net.Conn].
//
// The peer does not learn about the stream until something is written to it,
// so this costs no round trip.
func (c *Conn) OpenConn(ctx context.Context) (*StreamConn, error) {
	send, recv, err := c.OpenBi(ctx)
	if err != nil {
		return nil, err
	}
	return c.streamConn(send, recv)
}

// AcceptConn accepts the next bidirectional stream the peer opens and
// presents it as a [net.Conn].
func (c *Conn) AcceptConn(ctx context.Context) (*StreamConn, error) {
	send, recv, err := c.AcceptBi(ctx)
	if err != nil {
		return nil, err
	}
	return c.streamConn(send, recv)
}

func (c *Conn) streamConn(send *SendStream, recv *RecvStream) (*StreamConn, error) {
	remote, err := c.RemoteID()
	if err != nil {
		return nil, err
	}
	return &StreamConn{
		send:   send,
		recv:   recv,
		local:  c.local,
		remote: remote,
		closed: make(chan struct{}),
	}, nil
}

// Read reads from the stream, returning [io.EOF] at its clean end.
func (c *StreamConn) Read(p []byte) (int, error) {
	n, err := c.recv.Read(p)
	return n, c.closeAware(err)
}

// Write writes all of p unless a deadline stops it first.
func (c *StreamConn) Write(p []byte) (int, error) {
	n, err := c.send.Write(p)
	return n, c.closeAware(err)
}

// closeAware reports a call that Close interrupted as [net.ErrClosed], which
// is what a net.Conn caller expects. Close expires the deadlines to get the
// stream back from whatever is holding it, and the timeout that produces is
// an implementation detail rather than something the caller asked for.
func (c *StreamConn) closeAware(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	select {
	case <-c.closed:
		if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, ErrClosed) {
			return net.ErrClosed
		}
	default:
	}
	return err
}

// CloseWrite ends this side of the stream while leaving the other open, so
// the peer reads [io.EOF] and can still reply. It is what a protocol that
// says "that is all I have to send" needs, and what [StreamConn.Close] is
// not: net/http and crypto/ssh both reach for it through the same
// interface{ CloseWrite() error } assertion that *net.TCPConn satisfies.
func (c *StreamConn) CloseWrite() error {
	return c.send.Close()
}

// Close ends the stream in both directions. It is idempotent, and safe to
// call while a read or write is in flight: those calls return
// [net.ErrClosed].
func (c *StreamConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.closeErr = errors.Join(c.recv.Close(), c.send.Close())
	})
	return c.closeErr
}

// Reset abandons the stream, telling the peer the given application error
// code and discarding anything not yet delivered. It is how a caller says the
// exchange failed rather than finished, which [StreamConn.Close] cannot: a
// clean finish tells the peer the bytes it has are all of them.
func (c *StreamConn) Reset(ctx context.Context, code uint64) error {
	c.closeOnce.Do(func() {
		close(c.closed)
		// Same reason Close expires them: a call in flight holds the stream
		// inside the library and the teardown waits behind it.
		_ = c.recv.SetReadDeadline(pastDeadline)
		_ = c.send.SetWriteDeadline(pastDeadline)
		c.closeErr = errors.Join(c.recv.Stop(ctx, code), c.send.Reset(ctx, code))
	})
	return c.closeErr
}

// LocalAddr is the endpoint this side of the stream answers as.
func (c *StreamConn) LocalAddr() net.Addr { return Addr{ID: c.local} }

// RemoteAddr is the peer's endpoint, which the QUIC handshake authenticated.
// A server can build a principal from it without asking the client who it is.
func (c *StreamConn) RemoteAddr() net.Addr { return Addr{ID: c.remote} }

// SetDeadline sets both the read and the write deadline.
func (c *StreamConn) SetDeadline(t time.Time) error {
	return errors.Join(c.SetReadDeadline(t), c.SetWriteDeadline(t))
}

// SetReadDeadline bounds reads, including one already blocked.
func (c *StreamConn) SetReadDeadline(t time.Time) error {
	return c.recv.SetReadDeadline(t)
}

// SetWriteDeadline bounds writes, including one already blocked.
func (c *StreamConn) SetWriteDeadline(t time.Time) error {
	return c.send.SetWriteDeadline(t)
}

// ListenOptions configures a [Listener].
type ListenOptions struct {
	// Authorize decides whether a peer may proceed. It runs once per
	// connection, after the handshake has proven the peer's identity and
	// before any stream of theirs is accepted, so a refused peer never
	// reaches the server above.
	//
	// Returning an error refuses the connection; the error's text is the
	// close reason the peer reads, which is the only place a refusal can
	// explain itself. Nil accepts every peer that reaches the endpoint --
	// a listener, unlike an application, has no idea who ought to be allowed.
	Authorize func(*Conn) error
}

// Listener presents the streams peers open on an endpoint as a
// [net.Listener], so an ordinary server serves them unchanged.
//
// One listener per endpoint: accepting connections is single-consumer, so two
// listeners on one endpoint would take each other's peers.
type Listener struct {
	endpoint *Endpoint
	opts     ListenOptions
	conns    chan *StreamConn

	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once
	errMu     sync.Mutex
	err       error
}

// Listener starts accepting on this endpoint. Nothing is accepted until it is
// called, and [Listener.Close] stops it again.
func (e *Endpoint) Listener(opts ListenOptions) *Listener {
	ctx, cancel := context.WithCancel(context.Background())
	l := &Listener{
		endpoint: e,
		opts:     opts,
		conns:    make(chan *StreamConn),
		ctx:      ctx,
		cancel:   cancel,
	}
	go l.accept()
	return l
}

// accept runs on one goroutine, which is what [Endpoint.Accept] requires.
// Everything a connection then needs happens on its own.
func (l *Listener) accept() {
	for {
		conn, err := l.endpoint.Accept(l.ctx)
		if err != nil {
			// Only a closed endpoint or a closed listener ends the loop.
			// Every other failure belongs to one peer -- an alpn this
			// endpoint does not serve, a peer that vanished mid-handshake --
			// and taking the listener down with it would let any client end
			// service for all of them. net.TCPListener does not behave that
			// way and neither does this.
			if l.ctx.Err() != nil || errors.Is(err, ErrClosed) {
				l.fail(err)
				return
			}
			continue
		}
		go l.serve(conn)
	}
}

func (l *Listener) serve(conn *Conn) {
	if l.opts.Authorize != nil {
		if err := l.opts.Authorize(conn); err != nil {
			_ = conn.CloseWithError(refusedCode, err.Error())
			return
		}
	}
	for {
		stream, err := conn.AcceptConn(l.ctx)
		if err != nil {
			return
		}
		select {
		case l.conns <- stream:
		case <-l.ctx.Done():
			_ = stream.Close()
			return
		}
	}
}

// Accept returns the next stream any peer has opened. It reports
// [net.ErrClosed] once the listener is closed.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.ctx.Done():
		return nil, l.acceptErr()
	}
}

// Close stops accepting. It does not close the endpoint: an endpoint outlives
// any one listener, and a caller who wants it closed closes it.
func (l *Listener) Close() error {
	l.closeOnce.Do(l.cancel)
	return nil
}

// Addr is the endpoint peers dial to reach this listener.
func (l *Listener) Addr() net.Addr { return Addr{ID: l.endpoint.ID()} }

func (l *Listener) fail(err error) {
	l.errMu.Lock()
	if l.err == nil {
		// A listener that was closed reports the closing, not whatever the
		// accept in flight made of it.
		if l.ctx.Err() != nil {
			err = net.ErrClosed
		}
		l.err = err
	}
	l.errMu.Unlock()
	_ = l.Close()
}

func (l *Listener) acceptErr() error {
	l.errMu.Lock()
	defer l.errMu.Unlock()
	if l.err != nil {
		return l.err
	}
	return net.ErrClosed
}
