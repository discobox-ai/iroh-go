package iroh

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/discobox-ai/iroh-go/internal/ffi"
)

// deadline holds an optional per-stream deadline, so that the io.Reader and
// io.Writer methods -- which take no context -- can still be bounded, the
// same way net.Conn does it.
//
// net.Conn requires a deadline to reach a call that is *already* blocked, not
// merely bound the next one: net/http aborts a hijacked connection's pending
// background read by setting a deadline in the past and waiting for that read
// to return. So every call registers the context it is using, and [set]
// cancels the registered ones. The caller sees context.Canceled, derives a
// fresh context from the new deadline, and carries on -- or stops immediately,
// because the new deadline has already passed.
type deadline struct {
	mu sync.Mutex
	t  time.Time
	// armed holds the contexts of the calls currently in flight. A stream has
	// at most one read and one write outstanding, plus a concurrent Close, so
	// this stays tiny.
	armed map[uint64]context.CancelFunc
	next  uint64
}

func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	d.t = t
	armed := make([]context.CancelFunc, 0, len(d.armed))
	for _, cancel := range d.armed {
		armed = append(armed, cancel)
	}
	d.mu.Unlock()

	// Outside the lock: cancelling wakes a call that will re-arm.
	for _, cancel := range armed {
		cancel()
	}
}

// arm derives a context honouring the current deadline and registers it, so
// that a later [set] interrupts the call using it. The returned release func
// must always be called.
func (d *deadline) arm() (context.Context, func()) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if d.t.IsZero() {
		ctx, cancel = context.WithCancel(context.Background())
	} else {
		ctx, cancel = context.WithDeadline(context.Background(), d.t)
	}

	id := d.next
	d.next++
	if d.armed == nil {
		d.armed = make(map[uint64]context.CancelFunc)
	}
	d.armed[id] = cancel

	return ctx, func() {
		d.mu.Lock()
		delete(d.armed, id)
		d.mu.Unlock()
		cancel()
	}
}

// pastDeadline is a deadline that has already expired, which is how a call
// already in flight is told to let go of the stream it is holding.
var pastDeadline = time.Unix(1, 0)

// deadlineErr maps the context error an expired deadline produces onto the
// error net.Conn callers expect.
//
// os.ErrDeadlineExceeded is returned bare rather than wrapped: net/http and
// most other callers recognise a timeout with a net.Error type assertion,
// which a wrapped error defeats.
func deadlineErr(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return os.ErrDeadlineExceeded
	}
	return err
}

// SendStream is the writable half of a QUIC stream. It implements
// [io.WriteCloser].
//
// Like every io.Writer, a SendStream is not safe for concurrent writers.
type SendStream struct {
	h        handle
	writeMu  sync.Mutex
	deadline deadline
}

func newSendStream(h uint64) *SendStream {
	s := &SendStream{}
	s.h.set(h)
	runtime.AddCleanup(s, ffi.SendFree, h)
	return s
}

// Write writes all of p, honouring any deadline set with
// [SendStream.SetWriteDeadline]. A deadline that passes -- including one set
// while this call is already blocked -- ends it with [os.ErrDeadlineExceeded],
// reporting how much of p was written.
func (s *SendStream) Write(p []byte) (int, error) {
	written := 0
	for {
		ctx, release := s.deadline.arm()
		n, err := s.WriteContext(ctx, p[written:])
		release()
		written += n
		if err == nil {
			return written, nil
		}
		// context.Canceled here means the deadline changed rather than
		// expired, so resume under the new one from where this left off.
		if errors.Is(err, context.Canceled) && written < len(p) {
			continue
		}
		return written, deadlineErr(err)
	}
}

// WriteContext writes all of p unless ctx is cancelled first, in which case
// it reports how much did make it.
//
// A cancelled write leaves the stream at a known position: an operation that
// completes reports what it wrote even when the cancellation raced it, and one
// that does not complete writes nothing. Either way the returned count is
// exact and writing may simply resume.
func (s *SendStream) WriteContext(ctx context.Context, p []byte) (int, error) {
	h, err := s.h.get()
	if err != nil {
		return 0, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	written := 0
	for written < len(p) {
		n, err := ffi.SendWrite(ctx, h, p[written:])
		written += int(n)
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, errKind(KindStream, "stream accepted no data")
		}
	}
	return written, nil
}

// SetWriteDeadline bounds Write calls, including one already in flight. A
// zero time clears it.
func (s *SendStream) SetWriteDeadline(t time.Time) error {
	s.deadline.set(t)
	return nil
}

// SetPriority sets this stream's relative priority among the connection's
// streams. Higher values are sent first.
func (s *SendStream) SetPriority(ctx context.Context, priority int32) error {
	h, err := s.h.get()
	if err != nil {
		return err
	}
	return ffi.SendSetPriority(ctx, h, priority)
}

// Stopped waits until the peer stops the stream, reporting the code it gave.
// The bool is false when the stream ended without being stopped.
func (s *SendStream) Stopped(ctx context.Context) (uint64, bool, error) {
	h, err := s.h.get()
	if err != nil {
		return 0, false, err
	}
	code, err := ffi.SendStopped(ctx, h)
	if err != nil {
		return 0, false, err
	}
	if code == ffi.NoStopCode {
		return 0, false, nil
	}
	return code, true, nil
}

// Reset abandons the stream, discarding anything not yet delivered.
func (s *SendStream) Reset(ctx context.Context, code uint64) error {
	h := s.h.take()
	if h == 0 {
		return nil
	}
	defer ffi.SendFree(h)
	return ffi.SendReset(ctx, h, code)
}

// Close finishes the stream, signalling a clean end to the peer. It does not
// wait for the peer to acknowledge the data; use [SendStream.Stopped] for
// that. Close is idempotent, and safe to call from another goroutine while a
// write is in flight.
//
// It expires the write deadline without being bound by one. A write in flight
// holds the stream inside the library and the finish would wait behind it,
// while a Close that skipped the finish because the last write had timed out
// would leave the peer waiting for data that is never coming.
func (s *SendStream) Close() error {
	h := s.h.take()
	if h == 0 {
		return nil
	}
	defer ffi.SendFree(h)
	s.deadline.set(pastDeadline)
	return ffi.SendFinish(context.Background(), h)
}

// RecvStream is the readable half of a QUIC stream. It implements
// [io.ReadCloser].
type RecvStream struct {
	h        handle
	readMu   sync.Mutex
	deadline deadline
}

func newRecvStream(h uint64) *RecvStream {
	r := &RecvStream{}
	r.h.set(h)
	runtime.AddCleanup(r, ffi.RecvFree, h)
	return r
}

// Read reads into p, honouring any deadline set with
// [RecvStream.SetReadDeadline]. It returns [io.EOF] at the clean end of the
// stream. A deadline that passes -- including one set while this call is
// already blocked -- ends it with [os.ErrDeadlineExceeded], having consumed
// nothing.
func (r *RecvStream) Read(p []byte) (int, error) {
	for {
		ctx, release := r.deadline.arm()
		n, err := r.ReadContext(ctx, p)
		release()
		if n > 0 || err == nil {
			return n, err
		}
		// context.Canceled here means the deadline changed rather than
		// expired, and a cancelled read consumed nothing, so read again under
		// the new deadline.
		if errors.Is(err, context.Canceled) {
			continue
		}
		return 0, deadlineErr(err)
	}
}

// ReadContext reads into p unless ctx is cancelled first.
//
// A cancelled read consumes nothing, so the stream position is unchanged and
// reading may simply resume. A read that completes just as the cancellation
// arrives reports its bytes rather than losing them.
func (r *RecvStream) ReadContext(ctx context.Context, p []byte) (int, error) {
	h, err := r.h.get()
	if err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	r.readMu.Lock()
	defer r.readMu.Unlock()

	n, eof, err := ffi.RecvRead(ctx, h, p)
	if err != nil {
		return n, err
	}
	if eof {
		return n, io.EOF
	}
	return n, nil
}

// SetReadDeadline bounds Read calls, including one already in flight. A zero
// time clears it.
func (r *RecvStream) SetReadDeadline(t time.Time) error {
	r.deadline.set(t)
	return nil
}

// Stop tells the peer to stop sending on this stream and discards anything
// still buffered.
func (r *RecvStream) Stop(ctx context.Context, code uint64) error {
	h := r.h.take()
	if h == 0 {
		return nil
	}
	defer ffi.RecvFree(h)
	return ffi.RecvStop(ctx, h, code)
}

// Close stops the stream with a zero error code. It is idempotent, and safe
// to call from another goroutine while a read is in flight.
//
// Like [SendStream.Close] it expires the deadline first: a read in flight
// holds the stream inside the library, and a read with no deadline never ends
// on its own, so stopping it would wait forever behind it.
func (r *RecvStream) Close() error {
	r.deadline.set(pastDeadline)
	return r.Stop(context.Background(), 0)
}

var (
	_ io.WriteCloser = (*SendStream)(nil)
	_ io.ReadCloser  = (*RecvStream)(nil)
)
