//go:build cgo

package main

// builtWithCgo is true when ksar links libc. With cgo, Go creates threads
// through glibc, which makes syscalls (clone3, rseq, set_robust_list) with
// all signals blocked; the seccomp lockdown can't allow them safely and a
// blocked one kills ksar silently. Build with CGO_ENABLED=0 (make ksar).
const builtWithCgo = true
