package iroh

import "sync/atomic"

// handle wraps a library object id.
//
// Zeroing it on close makes use-after-close a clean Go-side error. The
// library also validates every handle, so the race between reading the id
// here and using it is safe: the worst case is the library reporting the
// same ErrClosed a moment later.
type handle struct {
	v atomic.Uint64
}

// set publishes an id into a freshly zeroed handle. The handle is never
// returned by value: atomic.Uint64 must not be copied.
func (h *handle) set(id uint64) { h.v.Store(id) }

func (h *handle) get() (uint64, error) {
	if id := h.v.Load(); id != 0 {
		return id, nil
	}
	return 0, ErrClosed
}

// take zeroes the handle and returns its previous value, or 0 if it was
// already taken. Exactly one caller ever sees a non-zero result, which is
// what makes Close idempotent.
func (h *handle) take() uint64 { return h.v.Swap(0) }
