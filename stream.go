package iroh

import (
	"context"
	"io"
	"runtime"
	"sync"
	"time"

	"github.com/discobox-ai/iroh-go/internal/ffi"
)

// deadline holds an optional per-stream deadline, so that the io.Reader and
// io.Writer methods -- which take no context -- can still be bounded, the
// same way net.Conn does it.
type deadline struct {
	mu sync.Mutex
	t  time.Time
}

func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	d.t = t
	d.mu.Unlock()
}

// context derives a context honouring the deadline. The returned cancel func
// must always be called.
func (d *deadline) context() (context.Context, context.CancelFunc) {
	d.mu.Lock()
	t := d.t
	d.mu.Unlock()
	if t.IsZero() {
		return context.Background(), func() {}
	}
	return context.WithDeadline(context.Background(), t)
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
// [SendStream.SetWriteDeadline].
func (s *SendStream) Write(p []byte) (int, error) {
	ctx, cancel := s.deadline.context()
	defer cancel()
	return s.WriteContext(ctx, p)
}

// WriteContext writes all of p unless ctx is cancelled first, in which case
// it reports how much did make it.
//
// A cancelled write leaves the stream at a known position: the underlying
// operation consumes nothing unless it completes, so the returned count is
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

// SetWriteDeadline bounds subsequent Write calls. A zero time clears it.
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
// that. Close is idempotent.
func (s *SendStream) Close() error {
	h := s.h.take()
	if h == 0 {
		return nil
	}
	defer ffi.SendFree(h)
	ctx, cancel := s.deadline.context()
	defer cancel()
	return ffi.SendFinish(ctx, h)
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
// stream.
func (r *RecvStream) Read(p []byte) (int, error) {
	ctx, cancel := r.deadline.context()
	defer cancel()
	return r.ReadContext(ctx, p)
}

// ReadContext reads into p unless ctx is cancelled first.
//
// A cancelled read consumes nothing, so the stream position is unchanged and
// reading may simply resume.
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

// SetReadDeadline bounds subsequent Read calls. A zero time clears it.
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

// Close stops the stream with a zero error code. It is idempotent.
func (r *RecvStream) Close() error {
	return r.Stop(context.Background(), 0)
}

var (
	_ io.WriteCloser = (*SendStream)(nil)
	_ io.ReadCloser  = (*RecvStream)(nil)
)
