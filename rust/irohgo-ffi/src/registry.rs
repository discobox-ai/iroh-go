//! A sharded handle table.
//!
//! Objects cross the FFI boundary as opaque `u64` handles rather than raw
//! pointers. That costs a hash lookup per call, but a stale handle then
//! produces a clean `ErrKind::Closed` instead of undefined behaviour --
//! worth it in a binding where double-`Close` from the Go side is a
//! realistic user bug.

use std::any::Any;
use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, OnceLock, RwLock};

use crate::error::{ErrKind, Error, Result};

/// Handle 0 is never allocated, so it always means "none".
pub const NULL_HANDLE: u64 = 0;

const SHARDS: usize = 16;

type Slot = Arc<dyn Any + Send + Sync>;

struct Registry {
    shards: Vec<RwLock<HashMap<u64, Slot>>>,
    next: AtomicU64,
}

fn registry() -> &'static Registry {
    static REG: OnceLock<Registry> = OnceLock::new();
    REG.get_or_init(|| Registry {
        shards: (0..SHARDS).map(|_| RwLock::new(HashMap::new())).collect(),
        next: AtomicU64::new(1),
    })
}

impl Registry {
    fn shard(&self, handle: u64) -> &RwLock<HashMap<u64, Slot>> {
        &self.shards[(handle as usize) % SHARDS]
    }
}

/// Reserves a handle id without publishing anything under it.
///
/// Used by the op table, which must know its own id before the task that
/// fills it can be spawned.
pub fn reserve() -> u64 {
    registry().next.fetch_add(1, Ordering::Relaxed)
}

/// Publishes `value` under a previously [`reserve`]d id.
pub fn publish<T: Any + Send + Sync>(handle: u64, value: Arc<T>) {
    let reg = registry();
    reg.shard(handle)
        .write()
        .expect("registry poisoned")
        .insert(handle, value);
}

/// Inserts `value` and returns its freshly allocated handle.
pub fn insert<T: Any + Send + Sync>(value: T) -> u64 {
    let handle = reserve();
    publish(handle, Arc::new(value));
    handle
}

/// Looks up a handle, checking that it holds a `T`.
pub fn get<T: Any + Send + Sync>(handle: u64) -> Result<Arc<T>> {
    if handle == NULL_HANDLE {
        return Err(Error::new(ErrKind::InvalidInput, "null handle"));
    }
    let reg = registry();
    let slot = reg
        .shard(handle)
        .read()
        .expect("registry poisoned")
        .get(&handle)
        .cloned()
        .ok_or_else(|| Error::closed(format!("handle {handle} is not live")))?;
    slot.downcast::<T>().map_err(|_| {
        Error::new(
            ErrKind::InvalidInput,
            format!("handle {handle} has wrong type"),
        )
    })
}

/// Drops a handle. Returns whether it was live.
pub fn remove(handle: u64) -> bool {
    if handle == NULL_HANDLE {
        return false;
    }
    let reg = registry();
    reg.shard(handle)
        .write()
        .expect("registry poisoned")
        .remove(&handle)
        .is_some()
}

/// Number of live handles. Exposed for leak tests.
pub fn len() -> usize {
    registry()
        .shards
        .iter()
        .map(|s| s.read().expect("registry poisoned").len())
        .sum()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn insert_get_remove_roundtrip() {
        let h = insert(String::from("hello"));
        assert_eq!(*get::<String>(h).unwrap(), "hello");
        assert!(remove(h));
        assert!(!remove(h));
        assert!(get::<String>(h).is_err());
    }

    #[test]
    fn wrong_type_is_rejected_not_reinterpreted() {
        let h = insert(String::from("hello"));
        let err = get::<u64>(h).unwrap_err();
        assert_eq!(err.kind, ErrKind::InvalidInput);
        remove(h);
    }

    #[test]
    fn null_handle_is_never_live() {
        assert!(get::<String>(NULL_HANDLE).is_err());
        assert!(!remove(NULL_HANDLE));
    }
}
