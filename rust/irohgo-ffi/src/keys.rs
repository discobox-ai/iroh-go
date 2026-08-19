//! Key material.
//!
//! Secret keys, endpoint ids and signatures are all fixed-size byte arrays,
//! so they cross as plain buffers and need no handles at all. Go models them
//! as `[32]byte` / `[64]byte` value types.

use iroh::{PublicKey, SecretKey, Signature};

use crate::error::{ErrKind, Error, Result};
use crate::ffi::{self, ffi_guard, ffi_try, FFI_OK};

pub const SECRET_KEY_LEN: usize = 32;
pub const ENDPOINT_ID_LEN: usize = 32;
pub const SIGNATURE_LEN: usize = 64;

fn secret_key(ptr: *const u8) -> Result<SecretKey> {
    let bytes = unsafe { ffi::slice(ptr, SECRET_KEY_LEN) };
    let arr: [u8; SECRET_KEY_LEN] = bytes
        .try_into()
        .map_err(|_| Error::new(ErrKind::KeyParsing, "secret key must be 32 bytes"))?;
    Ok(SecretKey::from_bytes(&arr))
}

pub fn endpoint_id(ptr: *const u8) -> Result<PublicKey> {
    let bytes = unsafe { ffi::slice(ptr, ENDPOINT_ID_LEN) };
    let arr: [u8; ENDPOINT_ID_LEN] = bytes
        .try_into()
        .map_err(|_| Error::new(ErrKind::KeyParsing, "endpoint id must be 32 bytes"))?;
    PublicKey::from_bytes(&arr).map_err(|e| Error::new(ErrKind::KeyParsing, e))
}

/// Generates a new random secret key into `out_key` (32 bytes).
#[no_mangle]
pub extern "C" fn iroh_secret_key_generate(out_key: *mut u8) {
    ffi_guard!((), {
        let key = SecretKey::generate();
        ffi::out_array(out_key, &key.to_bytes());
    })
}

/// Derives the public endpoint id (32 bytes) from a secret key (32 bytes).
#[no_mangle]
pub extern "C" fn iroh_secret_key_public(
    key: *const u8,
    out_id: *mut u8,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let key = ffi_try!(secret_key(key), out_err);
        ffi::out_array(out_id, key.public().as_bytes());
        FFI_OK
    })
}

/// Signs `msg` with `key`, writing 64 signature bytes to `out_sig`.
#[no_mangle]
pub extern "C" fn iroh_secret_key_sign(
    key: *const u8,
    msg: *const u8,
    msg_len: usize,
    out_sig: *mut u8,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let key = ffi_try!(secret_key(key), out_err);
        let msg = unsafe { ffi::slice(msg, msg_len) };
        ffi::out_array(out_sig, &key.sign(msg).to_bytes());
        FFI_OK
    })
}

/// Verifies a 64-byte signature over `msg` against a 32-byte endpoint id.
#[no_mangle]
pub extern "C" fn iroh_endpoint_id_verify(
    id: *const u8,
    msg: *const u8,
    msg_len: usize,
    sig: *const u8,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let id = ffi_try!(endpoint_id(id), out_err);
        let msg = unsafe { ffi::slice(msg, msg_len) };
        let sig_bytes = unsafe { ffi::slice(sig, SIGNATURE_LEN) };
        let arr: [u8; SIGNATURE_LEN] = ffi_try!(
            sig_bytes
                .try_into()
                .map_err(|_| Error::new(ErrKind::KeyParsing, "signature must be 64 bytes")),
            out_err
        );
        ffi_try!(
            id.verify(msg, &Signature::from_bytes(&arr))
                .map_err(|e| Error::new(ErrKind::KeyParsing, e)),
            out_err
        );
        FFI_OK
    })
}

/// Formats an endpoint id in iroh's canonical z-base-32 form.
#[no_mangle]
pub extern "C" fn iroh_endpoint_id_format(
    id: *const u8,
    out_str: *mut *mut u8,
    out_len: *mut usize,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let id = ffi_try!(endpoint_id(id), out_err);
        ffi::out_bytes(id.to_string().into_bytes(), out_str, out_len);
        FFI_OK
    })
}

/// Parses an endpoint id from its canonical z-base-32 form.
#[no_mangle]
pub extern "C" fn iroh_endpoint_id_parse(
    s: *const u8,
    s_len: usize,
    out_id: *mut u8,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let s = ffi_try!(unsafe { ffi::str_arg(s, s_len) }, out_err);
        let id = ffi_try!(
            s.parse::<PublicKey>()
                .map_err(|e| Error::new(ErrKind::KeyParsing, e)),
            out_err
        );
        ffi::out_array(out_id, id.as_bytes());
        FFI_OK
    })
}
