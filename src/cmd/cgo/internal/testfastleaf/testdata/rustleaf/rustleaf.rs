// A Rust function exported to C and called from Go through #cgo fastleaf.
//
// It must not unwind into Go, so it catches any panic at the boundary and
// reports it as an error code.

use std::panic;

/// Sums n bytes at p, multiplying by 31 per step (wrapping). A length
/// of usize::MAX makes it panic, to exercise the boundary.
/// Returns 0 on success, writing the sum to *out, or -1 if it panicked.
#[no_mangle]
pub extern "C" fn rust_sum(p: *const u8, n: usize, out: *mut u64) -> i32 {
    let r = panic::catch_unwind(|| {
        if n == usize::MAX {
            panic!("rust_sum: asked to panic");
        }
        let b = unsafe { std::slice::from_raw_parts(p, n) };
        let mut s: u64 = 0;
        for &c in b {
            s = s.wrapping_mul(31).wrapping_add(c as u64);
        }
        s
    });
    match r {
        Ok(s) => {
            unsafe { *out = s };
            0
        }
        Err(_) => -1,
    }
}
