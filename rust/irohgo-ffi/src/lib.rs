//! A flat C ABI over [iroh], designed to be driven from Go without CGO.
//!
//! The Go bindings load this library with `dlopen` via
//! [purego](https://github.com/ebitengine/purego) and call it through pure-Go
//! trampolines. Everything about the surface below follows from that:
//!
//! * Only scalars and pointers cross the boundary -- no structs by value, no
//!   floats -- so the ABI works on every architecture purego supports.
//! * Async work never calls back into Go. Operations return a `u64` op id
//!   immediately and post to one process-wide completion queue that a single
//!   Go thread drains. purego allows at most ~2000 process-lifetime
//!   callbacks, so a callback per operation is not an option.
//! * No Go pointer is retained past the return of the call it was passed to.
//!
//! See `ops.rs` for the completion queue and `ffi.rs` for the calling
//! conventions.

// Every entry point in this crate is an FFI boundary that receives raw
// pointers from a foreign caller and dereferences them. Marking each one
// `unsafe` would say nothing a C caller can observe, so the pointer contract
// is stated once, in `ffi.rs`, and enforced by the helpers there.
#![allow(clippy::not_unsafe_ptr_arg_deref)]

mod ffi;

mod addr;
mod conn;
mod endpoint;
mod error;
mod keys;
mod ops;
mod registry;
mod stream;

use error::{Error, Result};
use ffi::{ffi_guard, FFI_ERR, FFI_OK};
use ops::{OpValue, Wait};

/// Bumped whenever the ABI changes shape. The Go loader refuses a library
/// whose version it does not recognise, which turns a mismatched
/// library/bindings pair into a clear error instead of a crash.
pub const ABI_VERSION: u32 = 3;

#[no_mangle]
pub extern "C" fn iroh_abi_version() -> u32 {
    ABI_VERSION
}

/// The version of the iroh crate this library was built against.
///
/// Baked in from the lockfile at build time, so it reports what is actually
/// compiled in rather than what anyone remembered to write down. The Go side
/// exposes it as `iroh.IrohVersion`, and asserts it against the lockfile in a
/// test.
#[no_mangle]
pub extern "C" fn iroh_version(out_str: *mut *mut u8, out_len: *mut usize) {
    ffi_guard!((), {
        ffi::out_bytes(env!("IROH_VERSION").as_bytes().to_vec(), out_str, out_len);
    })
}

/// Starts the tokio runtime. Idempotent, and optional -- every other entry
/// point starts it on demand -- but calling it up front means bind failures
/// surface at load time.
#[no_mangle]
pub extern "C" fn iroh_init() -> i32 {
    ffi_guard!(FFI_ERR, {
        ops::runtime();
        FFI_OK
    })
}

/// Installs a tracing subscriber at the given level.
///
/// 0 off, 1 error, 2 warn, 3 info, 4 debug, 5 trace. Only the first call has
/// an effect, since a process may only install one global subscriber.
#[no_mangle]
pub extern "C" fn iroh_set_log_level(level: i32) -> i32 {
    ffi_guard!(FFI_ERR, {
        let directive = match level {
            0 => "off",
            1 => "error",
            2 => "warn",
            3 => "info",
            4 => "debug",
            5 => "trace",
            _ => return FFI_ERR,
        };
        let filter = match tracing_subscriber::EnvFilter::try_new(directive) {
            Ok(f) => f,
            Err(_) => return FFI_ERR,
        };
        match tracing_subscriber::fmt().with_env_filter(filter).try_init() {
            Ok(()) => FFI_OK,
            Err(_) => FFI_ERR,
        }
    })
}

/// Frees a buffer handed out by any `out_str`/`out_bytes` parameter.
#[no_mangle]
pub extern "C" fn iroh_free_bytes(ptr: *mut u8, len: usize) {
    ffi_guard!((), unsafe { ffi::free_bytes(ptr, len) })
}

/// Number of live handles. Exposed so tests can assert nothing leaked.
#[no_mangle]
pub extern "C" fn iroh_debug_handle_count() -> u64 {
    ffi_guard!(0, registry::len() as u64)
}

// -- completion queue -------------------------------------------------------

/// Blocks until an operation completes.
///
/// Returns 1 with `out_op`/`out_status` filled, 0 on timeout, or -1 once
/// `iroh_completion_wake` has been called. A negative `timeout_ms` waits
/// indefinitely.
///
/// Exactly one Go thread calls this, for the lifetime of the process.
#[no_mangle]
pub extern "C" fn iroh_completion_wait(
    timeout_ms: i64,
    out_op: *mut u64,
    out_status: *mut i32,
) -> i32 {
    ffi_guard!(-1, {
        match ops::wait(timeout_ms) {
            Wait::Ready(op, status) => {
                ffi::out(out_op, op);
                ffi::out(out_status, status);
                1
            }
            Wait::Timeout => 0,
            Wait::Shutdown => -1,
        }
    })
}

/// Wakes the completion waiter and makes all future waits return -1.
#[no_mangle]
pub extern "C" fn iroh_completion_wake() {
    ffi_guard!((), ops::shutdown())
}

// -- operation results ------------------------------------------------------

/// Requests cancellation of an in-flight operation.
///
/// Aborts the underlying task, so the iroh operation really is cancelled
/// rather than merely abandoned. The op still posts exactly one completion.
#[no_mangle]
pub extern "C" fn iroh_op_cancel(op: u64) {
    ffi_guard!((), {
        if let Ok(op) = ops::get(op) {
            op.cancel();
        }
    })
}

/// Drops an operation. Any unclaimed result it holds is released.
#[no_mangle]
pub extern "C" fn iroh_op_free(op: u64) {
    ffi_guard!((), {
        registry::remove(op);
    })
}

fn take(op: u64) -> Result<OpValue> {
    ops::get(op)?.take_result()
}

/// Collects a result that is a single handle.
#[no_mangle]
pub extern "C" fn iroh_op_result_handle(op: u64, out: *mut u64, out_err: *mut u64) -> i32 {
    ffi_guard!(-1, {
        match take(op) {
            Ok(OpValue::Handle(h)) => {
                ffi::out(out, h);
                FFI_OK
            }
            Ok(_) => ffi::store_err(Error::internal("operation did not yield a handle"), out_err),
            Err(e) => ffi::store_err(e, out_err),
        }
    })
}

/// Collects a result that is a pair of handles, as produced by the
/// bidirectional stream operations.
#[no_mangle]
pub extern "C" fn iroh_op_result_handle2(
    op: u64,
    out_a: *mut u64,
    out_b: *mut u64,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        match take(op) {
            Ok(OpValue::Handle2(a, b)) => {
                ffi::out(out_a, a);
                ffi::out(out_b, b);
                FFI_OK
            }
            Ok(_) => ffi::store_err(
                Error::internal("operation did not yield a handle pair"),
                out_err,
            ),
            Err(e) => ffi::store_err(e, out_err),
        }
    })
}

/// Collects a scalar result.
#[no_mangle]
pub extern "C" fn iroh_op_result_u64(op: u64, out: *mut u64, out_err: *mut u64) -> i32 {
    ffi_guard!(-1, {
        match take(op) {
            Ok(OpValue::U64(v)) => {
                ffi::out(out, v);
                FFI_OK
            }
            Ok(OpValue::Unit) => {
                ffi::out(out, 0);
                FFI_OK
            }
            Ok(_) => ffi::store_err(Error::internal("operation did not yield a scalar"), out_err),
            Err(e) => ffi::store_err(e, out_err),
        }
    })
}

/// Collects a byte-buffer result.
///
/// `out_eof` is set to 1 when the operation reached a clean end of stream,
/// in which case no buffer is produced. Free the buffer with
/// `iroh_free_bytes`.
#[no_mangle]
pub extern "C" fn iroh_op_result_bytes(
    op: u64,
    out_ptr: *mut *mut u8,
    out_len: *mut usize,
    out_eof: *mut i32,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        ffi::out(out_eof, 0);
        match take(op) {
            Ok(OpValue::Bytes(bytes)) => {
                ffi::out_bytes(bytes, out_ptr, out_len);
                FFI_OK
            }
            Ok(OpValue::Eof) => {
                ffi::out(out_eof, 1);
                ffi::out_bytes(Vec::new(), out_ptr, out_len);
                FFI_OK
            }
            Ok(OpValue::Unit) => {
                ffi::out_bytes(Vec::new(), out_ptr, out_len);
                FFI_OK
            }
            Ok(_) => ffi::store_err(Error::internal("operation did not yield bytes"), out_err),
            Err(e) => ffi::store_err(e, out_err),
        }
    })
}

/// Collects the failure of an operation whose completion status was not OK.
#[no_mangle]
pub extern "C" fn iroh_op_result_err(op: u64, out_err: *mut u64) -> i32 {
    ffi_guard!(-1, {
        match take(op) {
            Ok(_) => FFI_OK,
            Err(e) => ffi::store_err(e, out_err),
        }
    })
}

// -- errors -----------------------------------------------------------------

/// Consumes an error handle, yielding its kind and message.
///
/// The message buffer is owned by the caller and freed with
/// `iroh_free_bytes`.
#[no_mangle]
pub extern "C" fn iroh_error_take(
    err: u64,
    out_kind: *mut i32,
    out_msg: *mut *mut u8,
    out_len: *mut usize,
) {
    ffi_guard!((), {
        match registry::get::<Error>(err) {
            Ok(e) => {
                ffi::out(out_kind, e.kind as i32);
                ffi::out_bytes(e.msg.clone().into_bytes(), out_msg, out_len);
            }
            Err(_) => {
                ffi::out(out_kind, error::ErrKind::Internal as i32);
                ffi::out_bytes(b"unknown error handle".to_vec(), out_msg, out_len);
            }
        }
        registry::remove(err);
    })
}
