//! Tokio runtime, async operation table, and the completion queue.
//!
//! Every operation that awaits returns a `u64` op id immediately and later
//! pushes exactly one entry onto a single process-wide completion queue. The
//! Go side runs one dedicated OS thread blocked in `iroh_completion_wait`
//! and fans results out over Go channels.
//!
//! This shape exists because purego offers at most ~2000 process-lifetime
//! callbacks and gives the Go scheduler no visibility into blocking C calls.
//! A callback-per-operation design would exhaust the former; a
//! blocking-call-per-operation design would burn an OS thread per in-flight
//! stream read. One queue costs one parked thread, total.

use std::collections::VecDeque;
use std::future::Future;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Condvar, Mutex, OnceLock};
use std::time::Duration;

use tokio::runtime::Runtime;
use tokio::task::AbortHandle;

use crate::error::{ErrKind, Error, Result, COMPLETION_CANCELLED, COMPLETION_ERR, COMPLETION_OK};
use crate::registry;

/// The value an operation resolves to. Deliberately scalar-or-bytes only:
/// nothing here is a struct passed by value, which keeps the ABI usable on
/// purego's tier-2 architectures.
pub enum OpValue {
    Unit,
    Handle(u64),
    Handle2(u64, u64),
    U64(u64),
    Bytes(Vec<u8>),
    /// A read that hit the clean end of a stream. Distinct from an empty
    /// `Bytes` so Go can return `io.EOF` without guessing.
    Eof,
}

impl OpValue {
    /// Drops any registry handles this value owns.
    ///
    /// Needed when an operation completes successfully but its result is
    /// discarded because cancellation won the race -- otherwise the handle
    /// it created would leak.
    fn release_handles(&self) {
        match *self {
            OpValue::Handle(h) => {
                registry::remove(h);
            }
            OpValue::Handle2(a, b) => {
                registry::remove(a);
                registry::remove(b);
            }
            _ => {}
        }
    }
}

pub fn runtime() -> &'static Runtime {
    static RT: OnceLock<Runtime> = OnceLock::new();
    RT.get_or_init(|| {
        tokio::runtime::Builder::new_multi_thread()
            .enable_all()
            .thread_name("iroh-go")
            .build()
            .expect("failed to build tokio runtime")
    })
}

struct Completions {
    queue: Mutex<VecDeque<(u64, i32)>>,
    cv: Condvar,
    shutdown: AtomicBool,
}

fn completions() -> &'static Completions {
    static C: OnceLock<Completions> = OnceLock::new();
    C.get_or_init(|| Completions {
        queue: Mutex::new(VecDeque::new()),
        cv: Condvar::new(),
        shutdown: AtomicBool::new(false),
    })
}

impl Completions {
    fn push(&self, op: u64, status: i32) {
        self.queue
            .lock()
            .expect("completion queue poisoned")
            .push_back((op, status));
        self.cv.notify_one();
    }
}

/// Result of [`wait`].
pub enum Wait {
    Ready(u64, i32),
    Timeout,
    Shutdown,
}

/// Blocks the calling thread until a completion is available.
///
/// A negative `timeout_ms` waits indefinitely.
pub fn wait(timeout_ms: i64) -> Wait {
    let c = completions();
    let mut queue = c.queue.lock().expect("completion queue poisoned");
    loop {
        if let Some((op, status)) = queue.pop_front() {
            return Wait::Ready(op, status);
        }
        if c.shutdown.load(Ordering::SeqCst) {
            return Wait::Shutdown;
        }
        if timeout_ms < 0 {
            queue = c.cv.wait(queue).expect("completion queue poisoned");
        } else {
            let (q, t) =
                c.cv.wait_timeout(queue, Duration::from_millis(timeout_ms as u64))
                    .expect("completion queue poisoned");
            queue = q;
            if t.timed_out() && queue.is_empty() {
                return if c.shutdown.load(Ordering::SeqCst) {
                    Wait::Shutdown
                } else {
                    Wait::Timeout
                };
            }
        }
    }
}

/// Wakes every waiter and makes all future waits return [`Wait::Shutdown`].
pub fn shutdown() {
    let c = completions();
    // Take the lock so a waiter cannot check `shutdown` and then miss the
    // notification by sleeping between the store and the notify.
    let _guard = c.queue.lock().expect("completion queue poisoned");
    c.shutdown.store(true, Ordering::SeqCst);
    c.cv.notify_all();
}

pub struct Op {
    id: u64,
    cancel_requested: AtomicBool,
    abort: Mutex<Option<AbortHandle>>,
    result: Mutex<Option<Result<OpValue>>>,
}

impl Op {
    fn new(id: u64) -> Self {
        Self {
            id,
            cancel_requested: AtomicBool::new(false),
            abort: Mutex::new(None),
            result: Mutex::new(None),
        }
    }

    /// Records the outcome and posts this operation's single completion.
    ///
    /// Called exactly once, by the supervisor in [`spawn`], so there is no
    /// race to arbitrate here.
    fn post(&self, r: Result<OpValue>) {
        let status = match &r {
            Ok(_) => COMPLETION_OK,
            Err(e) if e.kind == ErrKind::Cancelled => COMPLETION_CANCELLED,
            Err(_) => COMPLETION_ERR,
        };
        *self.result.lock().expect("op poisoned") = Some(r);
        completions().push(self.id, status);
    }

    /// Requests cancellation by aborting the task.
    ///
    /// Deliberately posts nothing. Whether the operation counts as cancelled
    /// is decided by whether the task had already finished, which only its
    /// `JoinHandle` knows -- see [`spawn`]. An earlier version answered that
    /// question with a flag here, and a read whose future had already taken
    /// bytes off the stream lost them when cancellation won the swap.
    pub fn cancel(&self) {
        self.cancel_requested.store(true, Ordering::SeqCst);
        if let Some(handle) = self.abort.lock().expect("op poisoned").as_ref() {
            handle.abort();
        }
    }

    /// Takes ownership of the outcome, leaving the op empty.
    ///
    /// Callers take the result exactly once: ownership of any handle or
    /// buffer inside it transfers to Go, which is what makes [`Op::drop`]
    /// safe to treat a still-present result as unclaimed.
    pub fn take_result(&self) -> Result<OpValue> {
        match self.result.lock().expect("op poisoned").take() {
            Some(r) => r,
            None => Err(Error::internal("operation has not completed")),
        }
    }
}

impl Drop for Op {
    fn drop(&mut self) {
        // Released without its result being read (Go gave up, or the op was
        // cancelled after producing a handle). Don't leak the handle.
        if let Some(Ok(v)) = self.result.lock().expect("op poisoned").as_ref() {
            v.release_handles();
        }
    }
}

/// Spawns `fut` on the runtime and returns its op id.
pub fn spawn<F>(fut: F) -> u64
where
    F: Future<Output = Result<OpValue>> + Send + 'static,
{
    let id = registry::reserve();
    let op = Arc::new(Op::new(id));
    registry::publish(id, op.clone());

    let join = runtime().spawn(fut);
    *op.abort.lock().expect("op poisoned") = Some(join.abort_handle());
    // A cancel that arrived before the abort handle was stored would have
    // found `None`; honour it now.
    if op.cancel_requested.load(Ordering::SeqCst) {
        join.abort();
    }

    // A supervisor posts the completion rather than the task itself, so that
    // "did this finish?" is answered by the JoinHandle instead of by a flag
    // racing the task. A task that completed its work reports that work even
    // if an abort arrived a moment later; only a task that really was stopped
    // before finishing reports cancellation.
    let sup = op.clone();
    runtime().spawn(async move {
        sup.post(match join.await {
            Ok(r) => r,
            Err(e) if e.is_cancelled() => {
                Err(Error::new(ErrKind::Cancelled, "operation cancelled"))
            }
            Err(_) => Err(Error::internal("operation panicked")),
        });
    });
    id
}

/// Completes an operation immediately, without touching the runtime.
///
/// Used for calls that are logically async in the Go API but can fail
/// synchronously during argument validation.
pub fn spawn_ready(r: Result<OpValue>) -> u64 {
    let id = registry::reserve();
    let op = Arc::new(Op::new(id));
    registry::publish(id, op.clone());
    op.post(r);
    id
}

pub fn get(handle: u64) -> Result<Arc<Op>> {
    registry::get::<Op>(handle)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// One test function on purpose: the completion queue is process-wide,
    /// so parallel tests would dequeue each other's entries.
    #[test]
    fn completion_queue_semantics() {
        // Success posts exactly one completion, and a late cancel adds none.
        let id = spawn(async { Ok(OpValue::U64(42)) });
        assert!(matches!(wait(5_000), Wait::Ready(op, COMPLETION_OK) if op == id));
        ops_get(id).cancel();
        assert!(matches!(wait(50), Wait::Timeout));
        assert!(matches!(ops_get(id).take_result(), Ok(OpValue::U64(42))));
        registry::remove(id);

        // Cancelling an in-flight op reports it as cancelled, not as an error.
        let id = spawn(async {
            tokio::time::sleep(Duration::from_secs(30)).await;
            Ok(OpValue::Unit)
        });
        ops_get(id).cancel();
        assert!(matches!(wait(5_000), Wait::Ready(op, COMPLETION_CANCELLED) if op == id));
        registry::remove(id);

        // A task that already produced a value reports it even though an
        // abort follows. Cancelling means "stop if you have not finished",
        // never "discard what you did" -- bytes a read took off a stream are
        // gone from it whatever Go decides afterwards.
        let id = spawn(async { Ok(OpValue::Bytes(b"already read".to_vec())) });
        let op = ops_get(id);
        // Let the work task finish, then abort the already-finished task.
        std::thread::sleep(Duration::from_millis(100));
        op.cancel();
        assert!(matches!(wait(5_000), Wait::Ready(o, COMPLETION_OK) if o == id));
        match op.take_result() {
            Ok(OpValue::Bytes(b)) => assert_eq!(b, b"already read"),
            _ => panic!("a finished operation lost its result to a late cancel"),
        }
        registry::remove(id);

        // A handle produced by an op whose result is never claimed is freed
        // when the op is released, rather than leaking in the registry.
        let before = registry::len();
        let inner = registry::insert(String::from("connection"));
        let id = spawn_ready(Ok(OpValue::Handle(inner)));
        assert!(matches!(wait(5_000), Wait::Ready(op, COMPLETION_OK) if op == id));
        registry::remove(id);
        assert!(registry::get::<String>(inner).is_err());
        assert_eq!(registry::len(), before);
    }

    fn ops_get(id: u64) -> Arc<Op> {
        get(id).expect("op should be live")
    }
}
