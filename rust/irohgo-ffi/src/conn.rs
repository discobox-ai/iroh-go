//! Connections: streams, datagrams, stats and shutdown.

use std::sync::Arc;

use bytes::Bytes;
use iroh::endpoint::{Connection, VarInt};

use crate::error::{ErrKind, Error, Result};
use crate::ffi::{self, ffi_guard, ffi_try, FFI_OK};
use crate::ops::{self, OpValue};
use crate::registry;
use crate::stream::{RecvHandle, SendHandle};

/// Number of `u64` slots `iroh_conn_stats` writes. Part of the ABI.
pub const STATS_LEN: usize = 6;

fn conn(handle: u64) -> Result<Arc<Connection>> {
    registry::get::<Connection>(handle)
}

fn stream_err(e: impl std::fmt::Display) -> Error {
    Error::new(ErrKind::Stream, e)
}

/// Opens a bidirectional stream. Yields `(send, recv)` handles.
#[no_mangle]
pub extern "C" fn iroh_conn_open_bi(handle: u64) -> u64 {
    ffi_guard!(0, {
        let conn = match conn(handle) {
            Ok(c) => c,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        ops::spawn(async move {
            let (send, recv) = conn.open_bi().await.map_err(stream_err)?;
            Ok(OpValue::Handle2(
                registry::insert(SendHandle::new(send)),
                registry::insert(RecvHandle::new(recv)),
            ))
        })
    })
}

/// Accepts the next inbound bidirectional stream. Yields `(send, recv)`.
#[no_mangle]
pub extern "C" fn iroh_conn_accept_bi(handle: u64) -> u64 {
    ffi_guard!(0, {
        let conn = match conn(handle) {
            Ok(c) => c,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        ops::spawn(async move {
            let (send, recv) = conn.accept_bi().await.map_err(stream_err)?;
            Ok(OpValue::Handle2(
                registry::insert(SendHandle::new(send)),
                registry::insert(RecvHandle::new(recv)),
            ))
        })
    })
}

/// Opens a unidirectional stream. Yields a send-stream handle.
#[no_mangle]
pub extern "C" fn iroh_conn_open_uni(handle: u64) -> u64 {
    ffi_guard!(0, {
        let conn = match conn(handle) {
            Ok(c) => c,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        ops::spawn(async move {
            let send = conn.open_uni().await.map_err(stream_err)?;
            Ok(OpValue::Handle(registry::insert(SendHandle::new(send))))
        })
    })
}

/// Accepts the next inbound unidirectional stream. Yields a recv handle.
#[no_mangle]
pub extern "C" fn iroh_conn_accept_uni(handle: u64) -> u64 {
    ffi_guard!(0, {
        let conn = match conn(handle) {
            Ok(c) => c,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        ops::spawn(async move {
            let recv = conn.accept_uni().await.map_err(stream_err)?;
            Ok(OpValue::Handle(registry::insert(RecvHandle::new(recv))))
        })
    })
}

/// Sends an unreliable datagram. Fails immediately if it does not fit.
#[no_mangle]
pub extern "C" fn iroh_conn_send_datagram(
    handle: u64,
    data: *const u8,
    data_len: usize,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let conn = ffi_try!(conn(handle), out_err);
        let data = Bytes::copy_from_slice(unsafe { ffi::slice(data, data_len) });
        ffi_try!(
            conn.send_datagram(data)
                .map_err(|e| Error::new(ErrKind::Datagram, e)),
            out_err
        );
        FFI_OK
    })
}

/// Waits for the next datagram. Yields its bytes.
#[no_mangle]
pub extern "C" fn iroh_conn_read_datagram(handle: u64) -> u64 {
    ffi_guard!(0, {
        let conn = match conn(handle) {
            Ok(c) => c,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        ops::spawn(async move {
            let data = conn
                .read_datagram()
                .await
                .map_err(|e| Error::new(ErrKind::Datagram, e))?;
            Ok(OpValue::Bytes(data.to_vec()))
        })
    })
}

/// Largest datagram this connection can currently send, or 0 if datagrams
/// are unsupported by the peer.
#[no_mangle]
pub extern "C" fn iroh_conn_max_datagram_size(
    handle: u64,
    out_size: *mut u64,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let conn = ffi_try!(conn(handle), out_err);
        ffi::out(out_size, conn.max_datagram_size().unwrap_or(0) as u64);
        FFI_OK
    })
}

/// Writes the peer's 32-byte endpoint id.
#[no_mangle]
pub extern "C" fn iroh_conn_remote_id(handle: u64, out_id: *mut u8, out_err: *mut u64) -> i32 {
    ffi_guard!(-1, {
        let conn = ffi_try!(conn(handle), out_err);
        ffi::out_array(out_id, conn.remote_id().as_bytes());
        FFI_OK
    })
}

/// Returns the negotiated ALPN.
#[no_mangle]
pub extern "C" fn iroh_conn_alpn(
    handle: u64,
    out_alpn: *mut *mut u8,
    out_len: *mut usize,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let conn = ffi_try!(conn(handle), out_err);
        ffi::out_bytes(conn.alpn().to_vec(), out_alpn, out_len);
        FFI_OK
    })
}

/// Fills `out` with [`STATS_LEN`] counters, in a fixed order:
/// tx datagrams, tx bytes, rx datagrams, rx bytes, lost packets, lost bytes.
///
/// A flat `u64` array rather than a struct: purego cannot pass structs by
/// value on every architecture we target.
#[no_mangle]
pub extern "C" fn iroh_conn_stats(
    handle: u64,
    out: *mut u64,
    out_cap: usize,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let conn = ffi_try!(conn(handle), out_err);
        if out.is_null() || out_cap < STATS_LEN {
            return ffi::store_err(
                Error::invalid(format!("stats buffer must hold {STATS_LEN} u64 values")),
                out_err,
            );
        }
        let stats = conn.stats();
        let values = [
            stats.udp_tx.datagrams,
            stats.udp_tx.bytes,
            stats.udp_rx.datagrams,
            stats.udp_rx.bytes,
            stats.lost_packets,
            stats.lost_bytes,
        ];
        unsafe { std::ptr::copy_nonoverlapping(values.as_ptr(), out, STATS_LEN) };
        FFI_OK
    })
}

/// Closes the connection immediately with an application error code.
#[no_mangle]
pub extern "C" fn iroh_conn_close(
    handle: u64,
    code: u64,
    reason: *const u8,
    reason_len: usize,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let conn = ffi_try!(conn(handle), out_err);
        let code = ffi_try!(
            VarInt::from_u64(code).map_err(|e| Error::invalid(format!("bad close code: {e}"))),
            out_err
        );
        conn.close(code, unsafe { ffi::slice(reason, reason_len) });
        FFI_OK
    })
}

/// Waits for the connection to close. Yields the reason as a string.
#[no_mangle]
pub extern "C" fn iroh_conn_closed(handle: u64) -> u64 {
    ffi_guard!(0, {
        let conn = match conn(handle) {
            Ok(c) => c,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        ops::spawn(async move {
            let reason = conn.closed().await;
            Ok(OpValue::Bytes(reason.to_string().into_bytes()))
        })
    })
}

/// Drops the connection handle.
#[no_mangle]
pub extern "C" fn iroh_conn_free(handle: u64) {
    ffi_guard!((), {
        registry::remove(handle);
    })
}
