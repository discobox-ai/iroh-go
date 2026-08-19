//! Error kinds crossing the FFI boundary.
//!
//! Errors never cross as Rust types. Each fallible entry point yields an
//! `i32` kind plus an owned UTF-8 message that Go copies and frees.

use std::fmt;

/// Stable numeric error kinds. These values are part of the ABI: the Go side
/// maps them to sentinel errors, so only ever append.
///
/// Some variants are not constructed yet but are already mapped on the Go
/// side, so their numbering must stay put.
#[allow(dead_code)]
#[repr(i32)]
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ErrKind {
    Internal = 0,
    InvalidInput = 1,
    KeyParsing = 2,
    TicketParsing = 3,
    Bind = 4,
    Connect = 5,
    Connection = 6,
    Stream = 7,
    Datagram = 8,
    Alpn = 9,
    Relay = 10,
    Closed = 11,
    Timeout = 12,
    Cancelled = 13,
}

#[derive(Debug, Clone)]
pub struct Error {
    pub kind: ErrKind,
    pub msg: String,
}

impl Error {
    pub fn new(kind: ErrKind, msg: impl fmt::Display) -> Self {
        Self {
            kind,
            msg: msg.to_string(),
        }
    }

    pub fn internal(msg: impl fmt::Display) -> Self {
        Self::new(ErrKind::Internal, msg)
    }

    pub fn invalid(msg: impl fmt::Display) -> Self {
        Self::new(ErrKind::InvalidInput, msg)
    }

    pub fn closed(msg: impl fmt::Display) -> Self {
        Self::new(ErrKind::Closed, msg)
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.msg)
    }
}

pub type Result<T> = std::result::Result<T, Error>;

/// Completion statuses pushed onto the completion queue.
pub const COMPLETION_OK: i32 = 0;
pub const COMPLETION_ERR: i32 = 1;
pub const COMPLETION_CANCELLED: i32 = 2;
