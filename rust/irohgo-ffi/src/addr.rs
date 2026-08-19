//! `EndpointAddr` encoding and tickets.
//!
//! Rather than exposing a builder object with a dozen setters, an
//! `EndpointAddr` crosses the boundary as a small line-oriented text form.
//! That keeps the ABI to two functions per direction and makes the wire
//! format directly testable from both sides.
//!
//! ```text
//! <z-base-32 endpoint id>
//! relay <url>
//! ip <socket addr>
//! custom <id> <hex payload>
//! ```

use std::net::SocketAddr;
use std::str::FromStr;

use iroh::{EndpointAddr, RelayUrl};
use iroh_base::{CustomAddr, TransportAddr};
use iroh_tickets::{endpoint::EndpointTicket, Ticket};

use crate::error::{ErrKind, Error, Result};
use crate::ffi::{self, ffi_guard, ffi_try, FFI_OK};

pub fn encode_addr(addr: &EndpointAddr) -> String {
    let mut out = addr.id.to_string();
    for transport in &addr.addrs {
        match transport {
            TransportAddr::Relay(url) => {
                out.push_str("\nrelay ");
                out.push_str(url.as_str());
            }
            TransportAddr::Ip(sock) => {
                out.push_str("\nip ");
                out.push_str(&sock.to_string());
            }
            TransportAddr::Custom(custom) => {
                out.push_str("\ncustom ");
                out.push_str(&custom.id().to_string());
                out.push(' ');
                out.push_str(&hex(custom.data()));
            }
            // TransportAddr is #[non_exhaustive]; a newer iroh may add
            // variants we cannot represent yet, so drop them rather than
            // failing to encode the address at all.
            _ => {}
        }
    }
    out
}

pub fn decode_addr(text: &str) -> Result<EndpointAddr> {
    let mut lines = text.lines();
    let id_line = lines
        .next()
        .ok_or_else(|| Error::invalid("endpoint address is empty"))?;
    let id = id_line
        .trim()
        .parse()
        .map_err(|e| Error::new(ErrKind::KeyParsing, e))?;

    let mut addrs = Vec::new();
    for line in lines {
        let line = line.trim();
        if line.is_empty() {
            continue;
        }
        let (kind, rest) = line
            .split_once(' ')
            .ok_or_else(|| Error::invalid(format!("malformed address line: {line:?}")))?;
        match kind {
            "relay" => addrs.push(TransportAddr::Relay(
                RelayUrl::from_str(rest).map_err(|e| Error::new(ErrKind::Relay, e))?,
            )),
            "ip" => addrs.push(TransportAddr::Ip(SocketAddr::from_str(rest).map_err(
                |e| Error::invalid(format!("bad socket address {rest:?}: {e}")),
            )?)),
            "custom" => {
                let (id, data) = rest
                    .split_once(' ')
                    .ok_or_else(|| Error::invalid("custom address needs an id and payload"))?;
                let id: u64 = id
                    .parse()
                    .map_err(|e| Error::invalid(format!("bad custom address id: {e}")))?;
                addrs.push(TransportAddr::Custom(CustomAddr::from_parts(
                    id,
                    &unhex(data)?,
                )));
            }
            other => return Err(Error::invalid(format!("unknown address kind {other:?}"))),
        }
    }
    Ok(EndpointAddr::from_parts(id, addrs))
}

fn hex(bytes: &[u8]) -> String {
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        s.push_str(&format!("{b:02x}"));
    }
    s
}

fn unhex(s: &str) -> Result<Vec<u8>> {
    if !s.len().is_multiple_of(2) {
        return Err(Error::invalid("hex payload has odd length"));
    }
    (0..s.len())
        .step_by(2)
        .map(|i| {
            u8::from_str_radix(&s[i..i + 2], 16)
                .map_err(|e| Error::invalid(format!("bad hex payload: {e}")))
        })
        .collect()
}

/// Validates an address in text form, returning it normalised.
///
/// Go calls this so that a malformed address is reported where the user built
/// it rather than deep inside a later `Connect`.
#[no_mangle]
pub extern "C" fn iroh_addr_normalize(
    text: *const u8,
    text_len: usize,
    out_str: *mut *mut u8,
    out_len: *mut usize,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let text = ffi_try!(unsafe { ffi::str_arg(text, text_len) }, out_err);
        let addr = ffi_try!(decode_addr(text), out_err);
        ffi::out_bytes(encode_addr(&addr).into_bytes(), out_str, out_len);
        FFI_OK
    })
}

/// Encodes an address as an iroh ticket string.
#[no_mangle]
pub extern "C" fn iroh_ticket_encode(
    text: *const u8,
    text_len: usize,
    out_str: *mut *mut u8,
    out_len: *mut usize,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let text = ffi_try!(unsafe { ffi::str_arg(text, text_len) }, out_err);
        let addr = ffi_try!(decode_addr(text), out_err);
        let ticket = EndpointTicket::new(addr);
        ffi::out_bytes(ticket.encode_string().into_bytes(), out_str, out_len);
        FFI_OK
    })
}

/// Decodes an iroh ticket string into the address text form.
#[no_mangle]
pub extern "C" fn iroh_ticket_decode(
    s: *const u8,
    s_len: usize,
    out_str: *mut *mut u8,
    out_len: *mut usize,
    out_err: *mut u64,
) -> i32 {
    ffi_guard!(-1, {
        let s = ffi_try!(unsafe { ffi::str_arg(s, s_len) }, out_err);
        let ticket = ffi_try!(
            EndpointTicket::from_str(s.trim()).map_err(|e| Error::new(ErrKind::TicketParsing, e)),
            out_err
        );
        ffi::out_bytes(
            encode_addr(ticket.endpoint_addr()).into_bytes(),
            out_str,
            out_len,
        );
        FFI_OK
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use iroh::SecretKey;

    #[test]
    fn address_text_form_round_trips() {
        let id = SecretKey::generate().public();
        let addr = EndpointAddr::from_parts(
            id,
            [
                TransportAddr::Ip("127.0.0.1:4433".parse().unwrap()),
                TransportAddr::Ip("[::1]:4433".parse().unwrap()),
                TransportAddr::Relay("https://relay.example./".parse().unwrap()),
                TransportAddr::Custom(CustomAddr::from_parts(7, &[0xde, 0xad, 0xbe, 0xef])),
            ],
        );
        let decoded = decode_addr(&encode_addr(&addr)).unwrap();
        assert_eq!(decoded, addr);
    }

    #[test]
    fn id_only_address_round_trips() {
        let addr = EndpointAddr::new(SecretKey::generate().public());
        assert_eq!(decode_addr(&encode_addr(&addr)).unwrap(), addr);
    }

    #[test]
    fn malformed_input_is_rejected() {
        assert!(decode_addr("").is_err());
        assert!(decode_addr("not-an-id").is_err());
        let id = SecretKey::generate().public().to_string();
        assert!(decode_addr(&format!("{id}\nbogus 1.2.3.4:1")).is_err());
        assert!(decode_addr(&format!("{id}\nip not-an-addr")).is_err());
    }
}
