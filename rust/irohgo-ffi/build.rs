//! Bakes the locked iroh version into the library.
//!
//! `CARGO_PKG_VERSION` is this crate's own version, and iroh exposes no
//! version constant, so read what Cargo actually resolved. Doing it here
//! rather than writing the version down by hand means it cannot drift from
//! what is really compiled in.

use std::path::Path;

fn main() {
    let manifest = std::env::var("CARGO_MANIFEST_DIR").expect("CARGO_MANIFEST_DIR");
    let lock = Path::new(&manifest).join("..").join("Cargo.lock");
    println!("cargo:rerun-if-changed={}", lock.display());

    let version = std::fs::read_to_string(&lock)
        .ok()
        .and_then(|text| locked_version(&text, "iroh"))
        .unwrap_or_else(|| {
            panic!(
                "could not find the locked iroh version in {}",
                lock.display()
            )
        });

    println!("cargo:rustc-env=IROH_VERSION={version}");
}

/// Finds `version` in the `[[package]]` block whose `name` is `want`.
fn locked_version(lock: &str, want: &str) -> Option<String> {
    let mut in_package = false;
    let mut is_match = false;
    for line in lock.lines() {
        let line = line.trim();
        if line == "[[package]]" {
            in_package = true;
            is_match = false;
        } else if line.starts_with('[') {
            in_package = false;
        } else if in_package {
            if let Some(name) = line.strip_prefix("name = ") {
                is_match = name.trim_matches('"') == want;
            } else if is_match {
                if let Some(version) = line.strip_prefix("version = ") {
                    return Some(version.trim_matches('"').to_string());
                }
            }
        }
    }
    None
}
