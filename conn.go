package iroh

import (
	"context"
	"runtime"

	"github.com/discobox-ai/iroh-go/internal/ffi"
)

// Conn is an established QUIC connection to a remote endpoint.
//
// A connection carries any number of independent streams plus unreliable
// datagrams. It is safe for concurrent use.
type Conn struct {
	h handle
}

func newConn(h uint64) *Conn {
	c := &Conn{}
	c.h.set(h)
	runtime.AddCleanup(c, ffi.ConnFree, h)
	return c
}

// RemoteID returns the peer's endpoint id, proven by the TLS handshake.
func (c *Conn) RemoteID() (EndpointID, error) {
	var id EndpointID
	h, err := c.h.get()
	if err != nil {
		return id, err
	}
	if err := ffi.ConnRemoteID(h, id[:]); err != nil {
		return id, err
	}
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
	return newSendStream(send), newRecvStream(recv), nil
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
	return newSendStream(send), newRecvStream(recv), nil
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
	return newSendStream(send), nil
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
	return newRecvStream(recv), nil
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
