//! Shared plumbing for the C entry points.
//!
//! Conventions used by every `extern "C"` function in this crate:
//!
//! * Only scalars and pointers cross the boundary. Nothing is passed or
//!   returned by value as a struct, and no floats appear, so the ABI stays
//!   usable on purego's tier-2 architectures.
//! * Fallible synchronous calls return `0` on success and `1` on failure,
//!   writing an error handle into a trailing `out_err` out-parameter. Go then
//!   calls `iroh_error_take` to collect the kind and message. Errors are
//!   deliberately not stashed in a thread-local: Go goroutines migrate
//!   between OS threads, so a thread-local last-error would be unreadable.
//! * No Go pointer is ever retained past the return of the call it was
//!   passed to. Async operations copy their inputs first.

use std::panic::{catch_unwind, AssertUnwindSafe};

use crate::error::{Error, Result};
use crate::registry;

/// Runs `body`, converting a panic into `default` rather than letting it
/// unwind across the FFI boundary (which would be undefined behaviour).
macro_rules! ffi_guard {
    ($default:expr, $body:expr) => {
        match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| $body)) {
            Ok(value) => value,
            Err(_) => $default,
        }
    };
}

/// Unwraps a `Result`, or stores its error in `out_err` and returns `1`.
macro_rules! ffi_try {
    ($expr:expr, $out_err:expr) => {
        match $expr {
            Ok(value) => value,
            Err(err) => return $crate::ffi::store_err(err, $out_err),
        }
    };
}

pub(crate) use {ffi_guard, ffi_try};

pub const FFI_OK: i32 = 0;
pub const FFI_ERR: i32 = 1;

/// Publishes `err` as a handle in `out_err` and yields the failure status.
pub fn store_err(err: Error, out_err: *mut u64) -> i32 {
    if !out_err.is_null() {
        let handle = registry::insert(err);
        unsafe { *out_err = handle };
    }
    FFI_ERR
}

/// Borrows a caller-provided buffer.
///
/// # Safety
/// `ptr` must be valid for `len` bytes for the duration of the call, which is
/// guaranteed by the "no retained Go pointers" rule.
pub unsafe fn slice<'a>(ptr: *const u8, len: usize) -> &'a [u8] {
    if ptr.is_null() || len == 0 {
        &[]
    } else {
        std::slice::from_raw_parts(ptr, len)
    }
}

/// Borrows a caller-provided buffer as UTF-8.
///
/// # Safety
/// Same requirements as [`slice`].
pub unsafe fn str_arg<'a>(ptr: *const u8, len: usize) -> Result<&'a str> {
    std::str::from_utf8(slice(ptr, len)).map_err(|e| Error::invalid(format!("invalid utf-8: {e}")))
}

/// Hands ownership of `bytes` to the caller as a `(ptr, len)` pair.
///
/// The caller frees it with `iroh_free_bytes`. An empty buffer is reported as
/// a null pointer with length zero, which needs no free.
pub fn out_bytes(bytes: Vec<u8>, out_ptr: *mut *mut u8, out_len: *mut usize) {
    if bytes.is_empty() {
        unsafe {
            if !out_ptr.is_null() {
                *out_ptr = std::ptr::null_mut();
            }
            if !out_len.is_null() {
                *out_len = 0;
            }
        }
        return;
    }
    let boxed = bytes.into_boxed_slice();
    let len = boxed.len();
    let ptr = Box::into_raw(boxed) as *mut u8;
    unsafe {
        if !out_ptr.is_null() {
            *out_ptr = ptr;
        }
        if !out_len.is_null() {
            *out_len = len;
        }
    }
}

/// Reclaims a buffer handed out by [`out_bytes`].
///
/// # Safety
/// `ptr`/`len` must come from [`out_bytes`] and must not be freed twice.
pub unsafe fn free_bytes(ptr: *mut u8, len: usize) {
    if ptr.is_null() || len == 0 {
        return;
    }
    drop(Box::from_raw(std::ptr::slice_from_raw_parts_mut(ptr, len)));
}

/// Writes a scalar through an out-parameter, ignoring null.
pub fn out<T>(dst: *mut T, value: T) {
    if !dst.is_null() {
        unsafe { *dst = value };
    }
}

/// Copies a fixed-size array into a caller-provided buffer.
pub fn out_array(dst: *mut u8, src: &[u8]) {
    if dst.is_null() {
        return;
    }
    unsafe { std::ptr::copy_nonoverlapping(src.as_ptr(), dst, src.len()) };
}

/// Suppresses the unused-import warnings when only some helpers are used.
#[allow(dead_code)]
fn _assert_helpers_used() {
    let _ = catch_unwind(AssertUnwindSafe(|| ()));
}
