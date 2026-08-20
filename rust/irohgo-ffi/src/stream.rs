//! QUIC streams.
//!
//! Both `write` and `read` are exposed as *partial* operations that report
//! how much they moved, rather than write-all/read-to-end. That is
//! deliberate: quinn's `poll_write`/`poll_read` consume nothing when they
//! return `Pending`, so a partial op is cancel-safe -- dropping its future
//! cannot lose bytes. Go loops over them, and a cancelled `Write` leaves the
//! stream in a known position instead of an undefined one.

use std::sync::Arc;

use iroh::endpoint::{
    ConnectionError, ReadError, RecvStream, SendStream, StoppedError, VarInt, WriteError,
};
use tokio::sync::Mutex;

use crate::error::{ErrKind, Error, Result};
use crate::ffi::{self, ffi_guard};
use crate::ops::{self, OpValue};
use crate::registry;

/// Sentinel for "the peer did not send a stop code", since the ABI carries
/// no optionals.
pub const NO_STOP_CODE: u64 = u64::MAX;

pub struct SendHandle {
    inner: Mutex<SendStream>,
}

impl SendHandle {
    pub fn new(stream: SendStream) -> Self {
        Self {
            inner: Mutex::new(stream),
        }
    }
}

pub struct RecvHandle {
    inner: Mutex<RecvStream>,
}

impl RecvHandle {
    pub fn new(stream: RecvStream) -> Self {
        Self {
            inner: Mutex::new(stream),
        }
    }
}

fn send(handle: u64) -> Result<Arc<SendHandle>> {
    registry::get::<SendHandle>(handle)
}

fn recv(handle: u64) -> Result<Arc<RecvHandle>> {
    registry::get::<RecvHandle>(handle)
}

fn stream_err(e: impl std::fmt::Display) -> Error {
    Error::new(ErrKind::Stream, e)
}

/// A failure of the connection rather than of this stream. Every stream on
/// that connection is gone with it and the peer has to be redialled, which is
/// a different thing for the caller to do than retrying one stream -- so the
/// two do not share a kind.
fn conn_err(e: ConnectionError) -> Error {
    Error::new(ErrKind::Connection, e)
}

fn read_err(e: ReadError) -> Error {
    match e {
        ReadError::ConnectionLost(e) => conn_err(e),
        other => stream_err(other),
    }
}

fn write_err(e: WriteError) -> Error {
    match e {
        WriteError::ConnectionLost(e) => conn_err(e),
        other => stream_err(other),
    }
}

fn stopped_err(e: StoppedError) -> Error {
    match e {
        StoppedError::ConnectionLost(e) => conn_err(e),
        other => stream_err(other),
    }
}

/// Writes some of `data`. Yields the number of bytes accepted.
#[no_mangle]
pub extern "C" fn iroh_send_write(handle: u64, data: *const u8, data_len: usize) -> u64 {
    ffi_guard!(0, {
        // Copied before spawning: the Go caller's buffer is only valid for
        // the duration of this call.
        let data = unsafe { ffi::slice(data, data_len) }.to_vec();
        let stream = match send(handle) {
            Ok(s) => s,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        ops::spawn(async move {
            let n = stream
                .inner
                .lock()
                .await
                .write(&data)
                .await
                .map_err(write_err)?;
            Ok(OpValue::U64(n as u64))
        })
    })
}

/// Marks the stream finished. The peer sees a clean end of stream.
///
/// Idempotent: finishing an already-closed stream succeeds.
///
/// Like every stream mutation this is an op rather than a synchronous call,
/// because the stream lives behind an async mutex that an in-flight
/// read/write may hold. Go cancels any pending op on the stream first, which
/// releases that lock.
#[no_mangle]
pub extern "C" fn iroh_send_finish(handle: u64) -> u64 {
    ffi_guard!(0, {
        let stream = match send(handle) {
            Ok(s) => s,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        ops::spawn(async move {
            // finish/reset/stop can only fail with ClosedStream, meaning the
            // stream is already in the state the caller asked for. Reporting
            // that as an error would make Go's idempotent Close impossible.
            let _ = stream.inner.lock().await.finish();
            Ok(OpValue::Unit)
        })
    })
}

/// Abandons the stream, discarding anything not yet delivered. Idempotent.
#[no_mangle]
pub extern "C" fn iroh_send_reset(handle: u64, code: u64) -> u64 {
    ffi_guard!(0, {
        let stream = match send(handle) {
            Ok(s) => s,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        let code = match VarInt::from_u64(code) {
            Ok(c) => c,
            Err(e) => return ops::spawn_ready(Err(Error::invalid(format!("bad reset code: {e}")))),
        };
        ops::spawn(async move {
            let _ = stream.inner.lock().await.reset(code);
            Ok(OpValue::Unit)
        })
    })
}

/// Sets the stream's relative send priority.
#[no_mangle]
pub extern "C" fn iroh_send_set_priority(handle: u64, priority: i32) -> u64 {
    ffi_guard!(0, {
        let stream = match send(handle) {
            Ok(s) => s,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        ops::spawn(async move {
            stream
                .inner
                .lock()
                .await
                .set_priority(priority)
                .map_err(stream_err)?;
            Ok(OpValue::Unit)
        })
    })
}

/// Waits until the peer stops the stream. Yields the stop code, or
/// [`NO_STOP_CODE`] if it finished without one.
#[no_mangle]
pub extern "C" fn iroh_send_stopped(handle: u64) -> u64 {
    ffi_guard!(0, {
        let stream = match send(handle) {
            Ok(s) => s,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        ops::spawn(async move {
            let code = stream
                .inner
                .lock()
                .await
                .stopped()
                .await
                .map_err(stopped_err)?;
            Ok(OpValue::U64(code.map(u64::from).unwrap_or(NO_STOP_CODE)))
        })
    })
}

#[no_mangle]
pub extern "C" fn iroh_send_free(handle: u64) {
    ffi_guard!((), {
        registry::remove(handle);
    })
}

/// Reads up to `max_len` bytes. Yields the bytes read, or end-of-stream.
#[no_mangle]
pub extern "C" fn iroh_recv_read(handle: u64, max_len: usize) -> u64 {
    ffi_guard!(0, {
        let stream = match recv(handle) {
            Ok(s) => s,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        if max_len == 0 {
            return ops::spawn_ready(Err(Error::invalid("read length must be positive")));
        }
        ops::spawn(async move {
            let chunk = stream
                .inner
                .lock()
                .await
                .read_chunk(max_len)
                .await
                .map_err(read_err)?;
            Ok(match chunk {
                Some(bytes) => OpValue::Bytes(bytes.to_vec()),
                None => OpValue::Eof,
            })
        })
    })
}

/// Tells the peer to stop sending on this stream. Idempotent.
#[no_mangle]
pub extern "C" fn iroh_recv_stop(handle: u64, code: u64) -> u64 {
    ffi_guard!(0, {
        let stream = match recv(handle) {
            Ok(s) => s,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        let code = match VarInt::from_u64(code) {
            Ok(c) => c,
            Err(e) => return ops::spawn_ready(Err(Error::invalid(format!("bad stop code: {e}")))),
        };
        ops::spawn(async move {
            let _ = stream.inner.lock().await.stop(code);
            Ok(OpValue::Unit)
        })
    })
}

#[no_mangle]
pub extern "C" fn iroh_recv_free(handle: u64) {
    ffi_guard!((), {
        registry::remove(handle);
    })
}
