package ffi

import (
	"context"
	"runtime"
	"sync"
	"unsafe"
)

// The completion pump.
//
// One goroutine, locked to one OS thread, sits blocked inside
// iroh_completion_wait for the lifetime of the process and hands each
// finished operation to whoever is waiting on it. Everything else parks on a
// Go channel, so no matter how many streams are in flight the binding never
// blocks more than this single thread inside C.
//
// The alternative -- one blocking FFI call per operation -- would burn an OS
// thread per in-flight read, and a callback per operation would exhaust
// purego's ~2000 process-lifetime callbacks.

var pump = struct {
	mu sync.Mutex
	// waiters maps an op id to the channel its caller is parked on.
	waiters map[uint64]chan int32
	// unclaimed holds completions that arrived before their caller managed
	// to register. Without this, a fast operation could complete between
	// the call that started it and the registration that follows.
	unclaimed map[uint64]int32
}{
	waiters:   make(map[uint64]chan int32),
	unclaimed: make(map[uint64]int32),
}

func startPump() {
	ready := make(chan struct{})
	go func() {
		// The thread spends its life inside a blocking C call, so it must
		// not be reused for other goroutines. Unlock on the way out: a
		// goroutine that exits while locked takes its thread down with it,
		// and this one's thread has been through C.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		close(ready)
		for {
			var (
				op     uint64
				status int32
			)
			switch c.completionWait(-1, unsafe.Pointer(&op), unsafe.Pointer(&status)) {
			case 1:
				deliver(op, status)
			case 0:
				// Timeout; not reachable with an infinite wait, but harmless.
			default:
				return // the library is shutting down
			}
		}
	}()
	<-ready
}

func deliver(op uint64, status int32) {
	pump.mu.Lock()
	ch, ok := pump.waiters[op]
	if ok {
		delete(pump.waiters, op)
	} else {
		pump.unclaimed[op] = status
	}
	pump.mu.Unlock()

	if ok {
		ch <- status // buffered, so this never blocks the pump
	}
}

func register(op uint64) chan int32 {
	ch := make(chan int32, 1)
	pump.mu.Lock()
	if status, done := pump.unclaimed[op]; done {
		delete(pump.unclaimed, op)
		ch <- status
	} else {
		pump.waiters[op] = ch
	}
	pump.mu.Unlock()
	return ch
}

// await blocks until op completes or ctx is done.
//
// On cancellation it aborts the operation in Rust and still waits for the
// completion, because cancel is guaranteed to post exactly one. That drain
// is what keeps the waiter table from growing.
//
// A completion that already succeeded wins the race against the
// cancellation. An operation that has finished cannot be un-finished: its
// result exists, Rust drops whatever Go does not take, and a read's bytes are
// gone from the stream once that happens. Reporting the cancellation instead
// would silently lose them, so cancelling is "stop if you have not finished",
// not "discard what you did".
func await(ctx context.Context, op uint64) (int32, error) {
	if op == 0 {
		return 0, NewError(KindInternal, "iroh: operation could not be started")
	}
	ch := register(op)
	select {
	case status := <-ch:
		return status, nil
	case <-ctx.Done():
		c.opCancel(op)
		// The op stays alive on this path: the caller goes on to take its
		// result, and frees it there like any other completion.
		if status := <-ch; status == StatusOK {
			return status, nil
		}
		c.opFree(op)
		return 0, ctx.Err()
	}
}

// finish converts a completion status into an error, freeing the op.
func finish(op uint64, status int32) error {
	if status == StatusOK {
		return nil
	}
	var errh uint64
	c.opResultErr(op, unsafe.Pointer(&errh))
	err := takeError(errh)
	c.opFree(op)
	return err
}

// AwaitUnit waits for an operation that produces no value.
func AwaitUnit(ctx context.Context, op uint64) error {
	status, err := await(ctx, op)
	if err != nil {
		return err
	}
	if err := finish(op, status); err != nil {
		return err
	}
	c.opFree(op)
	return nil
}

// AwaitHandle waits for an operation that produces one handle.
func AwaitHandle(ctx context.Context, op uint64) (uint64, error) {
	status, err := await(ctx, op)
	if err != nil {
		return 0, err
	}
	if err := finish(op, status); err != nil {
		return 0, err
	}
	var handle, errh uint64
	rc := c.opResultHandle(op, unsafe.Pointer(&handle), unsafe.Pointer(&errh))
	c.opFree(op)
	if rc != 0 {
		return 0, takeError(errh)
	}
	return handle, nil
}

// AwaitHandle2 waits for an operation that produces a pair of handles, as the
// bidirectional stream operations do.
func AwaitHandle2(ctx context.Context, op uint64) (uint64, uint64, error) {
	status, err := await(ctx, op)
	if err != nil {
		return 0, 0, err
	}
	if err := finish(op, status); err != nil {
		return 0, 0, err
	}
	var a, b, errh uint64
	rc := c.opResultHandle2(op, unsafe.Pointer(&a), unsafe.Pointer(&b), unsafe.Pointer(&errh))
	c.opFree(op)
	if rc != 0 {
		return 0, 0, takeError(errh)
	}
	return a, b, nil
}

// AwaitU64 waits for an operation that produces a scalar.
func AwaitU64(ctx context.Context, op uint64) (uint64, error) {
	status, err := await(ctx, op)
	if err != nil {
		return 0, err
	}
	if err := finish(op, status); err != nil {
		return 0, err
	}
	var value, errh uint64
	rc := c.opResultU64(op, unsafe.Pointer(&value), unsafe.Pointer(&errh))
	c.opFree(op)
	if rc != 0 {
		return 0, takeError(errh)
	}
	return value, nil
}

// AwaitBytes waits for an operation that produces a buffer. The second result
// reports a clean end of stream, which is distinct from an empty buffer.
func AwaitBytes(ctx context.Context, op uint64) ([]byte, bool, error) {
	status, err := await(ctx, op)
	if err != nil {
		return nil, false, err
	}
	if err := finish(op, status); err != nil {
		return nil, false, err
	}
	var (
		ptr  unsafe.Pointer
		n    uintptr
		eof  int32
		errh uint64
	)
	rc := c.opResultBytes(op, unsafe.Pointer(&ptr), unsafe.Pointer(&n), unsafe.Pointer(&eof), unsafe.Pointer(&errh))
	c.opFree(op)
	if rc != 0 {
		return nil, false, takeError(errh)
	}
	return takeBytes(ptr, n), eof != 0, nil
}

// AwaitBytesInto is AwaitBytes for a caller-provided buffer. It avoids the
// intermediate allocation on the read path, which is the hot one.
func AwaitBytesInto(ctx context.Context, op uint64, dst []byte) (int, bool, error) {
	status, err := await(ctx, op)
	if err != nil {
		return 0, false, err
	}
	if err := finish(op, status); err != nil {
		return 0, false, err
	}
	var (
		ptr  unsafe.Pointer
		n    uintptr
		eof  int32
		errh uint64
	)
	rc := c.opResultBytes(op, unsafe.Pointer(&ptr), unsafe.Pointer(&n), unsafe.Pointer(&eof), unsafe.Pointer(&errh))
	c.opFree(op)
	if rc != 0 {
		return 0, false, takeError(errh)
	}
	copied := 0
	if ptr != nil && n > 0 {
		copied = copy(dst, unsafe.Slice((*byte)(ptr), n))
		c.freeBytes(ptr, n)
	}
	return copied, eof != 0, nil
}

// PendingOps reports how many operations are still waiting for a completion.
// Exposed for tests that assert cancellation reaps its operations.
func PendingOps() int {
	pump.mu.Lock()
	defer pump.mu.Unlock()
	return len(pump.waiters) + len(pump.unclaimed)
}
