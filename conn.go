package iroh

import (
	"context"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/iroh-go/internal/ffi"
)

// Conn is an established QUIC connection to a remote endpoint.
//
// A connection carries any number of independent streams plus unreliable
// datagrams. It is safe for concurrent use.
type Conn struct {
	h handle
	// endpoint is held so that Go cannot collect it while this connection is
	// still in use. Freeing an endpoint drops its driver, and every
	// connection on it dies with it -- reported, from the far side of the
	// FFI, as "endpoint driver future was dropped" in the middle of a working
	// exchange. A caller with a connection holds no endpoint, and a caller
	// with a stream holds neither, so the chain has to hold itself together.
	endpoint *Endpoint

	mu     sync.Mutex
	remote EndpointID
}

func newConn(h uint64, endpoint *Endpoint) *Conn {
	c := &Conn{endpoint: endpoint}
	c.h.set(h)
	runtime.AddCleanup(c, ffi.ConnFree, h)
	return c
}

// LocalID is the endpoint id this side of the connection answers as.
func (c *Conn) LocalID() EndpointID { return c.endpoint.ID() }

// RemoteID returns the peer's endpoint id, proven by the TLS handshake.
//
// A connection's peer cannot change, so the first successful lookup is
// remembered: a caller that identifies every stream by its peer would
// otherwise cross the FFI boundary once per stream to be told the same thing.
func (c *Conn) RemoteID() (EndpointID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.remote.IsZero() {
		return c.remote, nil
	}
	h, err := c.h.get()
	if err != nil {
		return EndpointID{}, err
	}
	var id EndpointID
	if err := ffi.ConnRemoteID(h, id[:]); err != nil {
		return EndpointID{}, err
	}
	c.remote = id
	return id, nil
}

// ALPN returns the protocol negotiated for this connection.
func (c *Conn) ALPN() ([]byte, error) {
	h, err := c.h.get()
	if err != nil {
		return nil, err
	}
	return ffi.ConnALPN(h)
}

// OpenBi opens a bidirectional stream.
//
// The peer does not learn about the stream until something is written to it.
func (c *Conn) OpenBi(ctx context.Context) (*SendStream, *RecvStream, error) {
	h, err := c.h.get()
	if err != nil {
		return nil, nil, err
	}
	send, recv, err := ffi.ConnOpenBi(ctx, h)
	if err != nil {
		return nil, nil, err
	}
	return newSendStream(send, c), newRecvStream(recv, c), nil
}

// AcceptBi waits for the peer to open a bidirectional stream.
func (c *Conn) AcceptBi(ctx context.Context) (*SendStream, *RecvStream, error) {
	h, err := c.h.get()
	if err != nil {
		return nil, nil, err
	}
	send, recv, err := ffi.ConnAcceptBi(ctx, h)
	if err != nil {
		return nil, nil, err
	}
	return newSendStream(send, c), newRecvStream(recv, c), nil
}

// OpenUni opens a unidirectional stream this side can only write to.
func (c *Conn) OpenUni(ctx context.Context) (*SendStream, error) {
	h, err := c.h.get()
	if err != nil {
		return nil, err
	}
	send, err := ffi.ConnOpenUni(ctx, h)
	if err != nil {
		return nil, err
	}
	return newSendStream(send, c), nil
}

// AcceptUni waits for the peer to open a unidirectional stream.
func (c *Conn) AcceptUni(ctx context.Context) (*RecvStream, error) {
	h, err := c.h.get()
	if err != nil {
		return nil, err
	}
	recv, err := ffi.ConnAcceptUni(ctx, h)
	if err != nil {
		return nil, err
	}
	return newRecvStream(recv, c), nil
}

// SendDatagram sends an unreliable, unordered datagram.
//
// It does not wait for capacity: if the datagram does not fit in the current
// window it fails immediately with an error wrapping [ErrDatagram]. Check
// [Conn.MaxDatagramSize] for the current limit.
func (c *Conn) SendDatagram(data []byte) error {
	h, err := c.h.get()
	if err != nil {
		return err
	}
	return ffi.ConnSendDatagram(h, data)
}

// ReadDatagram waits for the next datagram from the peer.
func (c *Conn) ReadDatagram(ctx context.Context) ([]byte, error) {
	h, err := c.h.get()
	if err != nil {
		return nil, err
	}
	return ffi.ConnReadDatagram(ctx, h)
}

// MaxDatagramSize is the largest datagram that currently fits, or 0 if the
// peer does not support datagrams.
func (c *Conn) MaxDatagramSize() (int, error) {
	h, err := c.h.get()
	if err != nil {
		return 0, err
	}
	n, err := ffi.ConnMaxDatagramSize(h)
	return int(n), err
}

// ConnStats are cumulative counters for a connection.
type ConnStats struct {
	SentDatagrams     uint64
	SentBytes         uint64
	ReceivedDatagrams uint64
	ReceivedBytes     uint64
	LostPackets       uint64
	LostBytes         uint64
}

// Stats returns the connection's current counters.
func (c *Conn) Stats() (ConnStats, error) {
	h, err := c.h.get()
	if err != nil {
		return ConnStats{}, err
	}
	v, err := ffi.ConnStats(h)
	if err != nil {
		return ConnStats{}, err
	}
	return ConnStats{
		SentDatagrams:     v[0],
		SentBytes:         v[1],
		ReceivedDatagrams: v[2],
		ReceivedBytes:     v[3],
		LostPackets:       v[4],
		LostBytes:         v[5],
	}, nil
}

// PathKind is how a network path carries a connection's packets.
type PathKind string

const (
	// PathIP is a direct path: UDP straight between the two endpoints.
	PathIP PathKind = "ip"
	// PathRelay is a path through a relay server. It is what carries a
	// connection until hole punching succeeds, and what carries it for good
	// between two endpoints that cannot reach each other directly.
	PathRelay PathKind = "relay"
	// PathCustom is a path over a transport the application supplied.
	PathCustom PathKind = "custom"
)

// Path is one open network path of a connection.
type Path struct {
	Kind PathKind
	// Selected reports whether this is the path application data is being
	// sent on. Exactly one open path is selected at a time.
	Selected bool
	// Remote is the far end of this path in text form: an ip:port for
	// [PathIP], a relay url for [PathRelay].
	Remote string
	// RTT is iroh's current round-trip estimate for this path.
	RTT time.Duration
}

// pathFields is the number of tab-separated fields in one path record, as
// iroh_conn_paths writes them.
const pathFields = 4

// Paths returns the connection's open network paths, which is how a caller
// learns whether it is talking to the peer directly or through a relay, and
// at what address.
//
// A connection usually has more than one: iroh opens on a relay, races the
// direct addresses it knows alongside it, and moves application data onto the
// best path that answers. So this is a snapshot of a moving picture -- a
// connection is typically relayed at the handshake and direct a moment later
// -- and a caller reporting how traffic actually flows should ask once the
// connection has carried a round trip rather than the instant it is
// established.
func (c *Conn) Paths() ([]Path, error) {
	h, err := c.h.get()
	if err != nil {
		return nil, err
	}
	text, err := ffi.ConnPaths(h)
	if err != nil {
		return nil, err
	}
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	paths := make([]Path, 0, len(lines))
	for _, line := range lines {
		fields := strings.SplitN(line, "\t", pathFields)
		if len(fields) != pathFields {
			return nil, errKind(KindInternal, "unparsable path %q", line)
		}
		micros, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return nil, errKind(KindInternal, "unparsable path rtt %q: %v", fields[2], err)
		}
		paths = append(paths, Path{
			Kind:     PathKind(fields[0]),
			Selected: fields[1] == "1",
			Remote:   fields[3],
			RTT:      time.Duration(micros) * time.Microsecond,
		})
	}
	return paths, nil
}

// Wait blocks until the connection closes and reports why.
//
// A connection closed cleanly by either side still produces an error here,
// describing the close; that is the only way to distinguish it from the many
// ways a connection can fail.
func (c *Conn) Wait(ctx context.Context) error {
	h, err := c.h.get()
	if err != nil {
		return err
	}
	reason, err := ffi.ConnClosed(ctx, h)
	if err != nil {
		return err
	}
	return &Error{Kind: KindConnection, Msg: reason}
}

// CloseWithError closes the connection immediately, telling the peer the
// given application error code and reason.
//
// Data still in flight is discarded. To make sure the peer received
// everything, finish the streams and wait for their acknowledgement first.
func (c *Conn) CloseWithError(code uint64, reason string) error {
	h := c.h.take()
	if h == 0 {
		return nil
	}
	defer ffi.ConnFree(h)
	return ffi.ConnClose(h, code, []byte(reason))
}

// Close closes the connection with a zero error code. It is idempotent.
func (c *Conn) Close() error { return c.CloseWithError(0, "") }
