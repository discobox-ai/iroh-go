//! Endpoint construction and lifecycle.

use std::net::SocketAddr;
use std::str::FromStr;
use std::sync::Mutex;

use iroh::endpoint::{presets, Builder};
use iroh::{Endpoint, RelayMode, RelayUrl, SecretKey};

use crate::addr::{decode_addr, encode_addr};
use crate::error::{ErrKind, Error, Result};
use crate::ffi::{self, ffi_guard, ffi_try, FFI_OK};
use crate::ops::{self, OpValue};
use crate::registry;

/// Preset selectors. These values are ABI.
pub const PRESET_EMPTY: i32 = 0;
pub const PRESET_MINIMAL: i32 = 1;
pub const PRESET_N0: i32 = 2;
pub const PRESET_N0_DISABLE_RELAY: i32 = 3;

/// Relay mode selectors. These values are ABI.
pub const RELAY_DEFAULT: i32 = 0;
pub const RELAY_DISABLED: i32 = 1;
pub const RELAY_STAGING: i32 = 2;
pub const RELAY_CUSTOM: i32 = 3;

/// Accumulated endpoint configuration.
///
/// Kept as plain data rather than a partially applied [`Builder`] because
/// `Builder`'s setters consume `self`, which does not survive being poked at
/// through a handle one field at a time.
#[derive(Default)]
struct Opts {
    preset: i32,
    secret_key: Option<SecretKey>,
    alpns: Vec<Vec<u8>>,
    relay_mode: Option<i32>,
    relay_urls: Vec<RelayUrl>,
    bind_addrs: Vec<SocketAddr>,
}

impl Opts {
    fn into_builder(self) -> Result<Builder> {
        let mut builder = match self.preset {
            PRESET_EMPTY => Builder::empty(),
            PRESET_MINIMAL => Builder::new(presets::Minimal),
            PRESET_N0_DISABLE_RELAY => Builder::new(presets::N0DisableRelay),
            PRESET_N0 => Builder::new(presets::N0),
            other => return Err(Error::invalid(format!("unknown preset {other}"))),
        };

        if let Some(key) = self.secret_key {
            builder = builder.secret_key(key);
        }
        if !self.alpns.is_empty() {
            builder = builder.alpns(self.alpns);
        }
        if let Some(mode) = self.relay_mode {
            builder = builder.relay_mode(match mode {
                RELAY_DEFAULT => RelayMode::Default,
                RELAY_DISABLED => RelayMode::Disabled,
                RELAY_STAGING => RelayMode::Staging,
                RELAY_CUSTOM => RelayMode::custom(self.relay_urls),
                other => return Err(Error::invalid(format!("unknown relay mode {other}"))),
            });
        }
        if !self.bind_addrs.is_empty() {
            // Presets install default IP transports; drop them so the
            // caller's explicit addresses are the only ones bound.
            builder = builder.clear_ip_transports();
            for addr in self.bind_addrs {
                builder = builder
                    .bind_addr(addr)
                    .map_err(|e| Error::new(ErrKind::Bind, e))?;
            }
        }
        Ok(builder)
    }
}

fn opts(handle: u64) -> Result<std::sync::Arc<Mutex<Opts>>> {
    registry::get::<Mutex<Opts>>(handle)
}

fn endpoint(handle: u64) -> Result<std::sync::Arc<Endpoint>> {
    registry::get::<Endpoint>(handle)
}

/// Creates an options object. Free with `iroh_options_free`, or hand it to
/// `iroh_endpoint_bind`, which consumes it.
#[no_mangle]
pub extern "C" fn iroh_options_new(preset: i32) -> u64 {
    ffi_guard!(0, {
        registry::insert(Mutex::new(Opts {
            preset,
            ..Default::default()
        }))
    })
}

#[no_mangle]
pub extern "C" fn iroh_options_free(handle: u64) {
    ffi_guard!((), {
        registry::remove(handle);
    })
}

#[no_mangle]
pub extern "C" fn iroh_options_set_secret_key(
    handle: u64,
    key: *const u8,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let bytes = unsafe { ffi::slice(key, 32) };
        let arr: [u8; 32] = ffi_try!(
            bytes
                .try_into()
                .map_err(|_| Error::new(ErrKind::KeyParsing, "secret key must be 32 bytes")),
            out_err
        );
        let opts = ffi_try!(opts(handle), out_err);
        opts.lock().expect("options poisoned").secret_key = Some(SecretKey::from_bytes(&arr));
        FFI_OK
    })
}

#[no_mangle]
pub extern "C" fn iroh_options_add_alpn(
    handle: u64,
    alpn: *const u8,
    alpn_len: usize,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let alpn = unsafe { ffi::slice(alpn, alpn_len) }.to_vec();
        if alpn.is_empty() {
            return ffi::store_err(Error::invalid("alpn must not be empty"), out_err);
        }
        let opts = ffi_try!(opts(handle), out_err);
        opts.lock().expect("options poisoned").alpns.push(alpn);
        FFI_OK
    })
}

#[no_mangle]
pub extern "C" fn iroh_options_set_relay_mode(handle: u64, mode: i32, out_err: *mut u64) -> i32 {
    ffi_guard!(-1, {
        let opts = ffi_try!(opts(handle), out_err);
        opts.lock().expect("options poisoned").relay_mode = Some(mode);
        FFI_OK
    })
}

#[no_mangle]
pub extern "C" fn iroh_options_add_relay_url(
    handle: u64,
    url: *const u8,
    url_len: usize,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let url = ffi_try!(unsafe { ffi::str_arg(url, url_len) }, out_err);
        let url = ffi_try!(
            RelayUrl::from_str(url).map_err(|e| Error::new(ErrKind::Relay, e)),
            out_err
        );
        let opts = ffi_try!(opts(handle), out_err);
        opts.lock().expect("options poisoned").relay_urls.push(url);
        FFI_OK
    })
}

#[no_mangle]
pub extern "C" fn iroh_options_add_bind_addr(
    handle: u64,
    addr: *const u8,
    addr_len: usize,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let text = ffi_try!(unsafe { ffi::str_arg(addr, addr_len) }, out_err);
        let addr = ffi_try!(
            SocketAddr::from_str(text)
                .map_err(|e| Error::invalid(format!("bad bind address {text:?}: {e}"))),
            out_err
        );
        let opts = ffi_try!(opts(handle), out_err);
        opts.lock().expect("options poisoned").bind_addrs.push(addr);
        FFI_OK
    })
}

/// Binds an endpoint. Consumes `options` whether or not the bind succeeds.
///
/// Yields an endpoint handle.
#[no_mangle]
pub extern "C" fn iroh_endpoint_bind(options: u64) -> u64 {
    ffi_guard!(0, {
        let opts = match opts(options) {
            Ok(o) => o,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        registry::remove(options);
        let opts = std::mem::take(&mut *opts.lock().expect("options poisoned"));

        ops::spawn(async move {
            let builder = opts.into_builder()?;
            let endpoint = builder
                .bind()
                .await
                .map_err(|e| Error::new(ErrKind::Bind, e))?;
            Ok(OpValue::Handle(registry::insert(endpoint)))
        })
    })
}

/// Writes the endpoint's own 32-byte id.
#[no_mangle]
pub extern "C" fn iroh_endpoint_id(handle: u64, out_id: *mut u8, out_err: *mut u64) -> i32 {
    ffi_guard!(-1, {
        let ep = ffi_try!(endpoint(handle), out_err);
        ffi::out_array(out_id, ep.id().as_bytes());
        FFI_OK
    })
}

/// Snapshots the endpoint's current address in text form.
///
/// Available as soon as the endpoint is bound, but only contains local
/// addresses until the endpoint comes online -- see `iroh_endpoint_online`.
#[no_mangle]
pub extern "C" fn iroh_endpoint_addr(
    handle: u64,
    out_str: *mut *mut u8,
    out_len: *mut usize,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let ep = ffi_try!(endpoint(handle), out_err);
        ffi::out_bytes(encode_addr(&ep.addr()).into_bytes(), out_str, out_len);
        FFI_OK
    })
}

/// Waits until the endpoint has connected to a relay.
///
/// Never resolves when relays are disabled or there is no WAN connection, so
/// the Go side always drives this with a context.
#[no_mangle]
pub extern "C" fn iroh_endpoint_online(handle: u64) -> u64 {
    ffi_guard!(0, {
        let ep = match endpoint(handle) {
            Ok(ep) => ep,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        ops::spawn(async move {
            ep.online().await;
            Ok(OpValue::Unit)
        })
    })
}

/// Returns the endpoint's home relay url, or an empty string if it has none.
#[no_mangle]
pub extern "C" fn iroh_endpoint_home_relay(
    handle: u64,
    out_str: *mut *mut u8,
    out_len: *mut usize,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let ep = ffi_try!(endpoint(handle), out_err);
        let url = ep
            .addr()
            .relay_urls()
            .next()
            .map(|u| u.to_string())
            .unwrap_or_default();
        ffi::out_bytes(url.into_bytes(), out_str, out_len);
        FFI_OK
    })
}

/// Dials `addr` (text form) with `alpn`. Yields a connection handle.
#[no_mangle]
pub extern "C" fn iroh_endpoint_connect(
    handle: u64,
    addr: *const u8,
    addr_len: usize,
    alpn: *const u8,
    alpn_len: usize,
) -> u64 {
    ffi_guard!(0, {
        // Copy every input before spawning: the Go caller's buffers are only
        // valid for the duration of this call.
        let addr = match unsafe { ffi::str_arg(addr, addr_len) }.and_then(decode_addr) {
            Ok(a) => a,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        let alpn = unsafe { ffi::slice(alpn, alpn_len) }.to_vec();
        let ep = match endpoint(handle) {
            Ok(ep) => ep,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        ops::spawn(async move {
            let conn = ep
                .connect(addr, &alpn)
                .await
                .map_err(|e| Error::new(ErrKind::Connect, e))?;
            Ok(OpValue::Handle(registry::insert(conn)))
        })
    })
}

/// Accepts the next inbound connection, completing its handshake.
///
/// Yields a connection handle, or `ErrKind::Closed` once the endpoint is
/// closed.
#[no_mangle]
pub extern "C" fn iroh_endpoint_accept(handle: u64) -> u64 {
    ffi_guard!(0, {
        let ep = match endpoint(handle) {
            Ok(ep) => ep,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        ops::spawn(async move {
            let incoming = ep
                .accept()
                .await
                .ok_or_else(|| Error::closed("endpoint is closed"))?;
            let conn = incoming
                .await
                .map_err(|e| Error::new(ErrKind::Connect, e))?;
            Ok(OpValue::Handle(registry::insert(conn)))
        })
    })
}

/// Closes the endpoint gracefully.
#[no_mangle]
pub extern "C" fn iroh_endpoint_close(handle: u64) -> u64 {
    ffi_guard!(0, {
        let ep = match endpoint(handle) {
            Ok(ep) => ep,
            Err(e) => return ops::spawn_ready(Err(e)),
        };
        ops::spawn(async move {
            ep.close().await;
            Ok(OpValue::Unit)
        })
    })
}

/// Drops the endpoint handle. Does not close the endpoint; call
/// `iroh_endpoint_close` first for a graceful shutdown.
#[no_mangle]
pub extern "C" fn iroh_endpoint_free(handle: u64) {
    ffi_guard!((), {
        registry::remove(handle);
    })
}
