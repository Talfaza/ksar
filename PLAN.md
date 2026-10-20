# Ksar

A local tool for running **untrusted code** inside microVMs, built directly on
Linux `/dev/kvm` (our own VMM in Go, no Firecracker or QEMU), managed from a
web page served on the user's own machine.

## Goals

- Run untrusted code in a hardware-isolated microVM on the user's machine.
- Let the user configure everything, but **nothing is open by default**.
- One Go binary: VMM engine + CLI + embedded local web UI.

## Secure defaults

| Setting        | Default                                   | User can choose                         |
|----------------|-------------------------------------------|-----------------------------------------|
| vCPUs / memory | 1 vCPU, 256 MB                            | Any amount                              |
| Timeout        | 60 s, then the VM is killed               | Longer or none                          |
| Image          | Minimal Linux rootfs                      | Any image (e.g. built from Docker images) |
| Disk           | Read-only base + throwaway writable layer | Persistent disk                         |
| Network        | **None**                                  | NAT, optional egress allowlist          |
| Display        | **None** (CLI only)                       | Framebuffer + virtio-input, or VNC      |
| Files          | Only what the user sends in               | Extra input/output disks                |
| Lifetime       | Destroyed after the run                   | Keep alive for multiple commands        |

## Architecture

```
Browser ──► http://127.0.0.1:PORT (token + Origin check)
                │
        ksar (Go, single binary)
        ├── web UI (embedded) + HTTP/WebSocket API
        ├── CLI (same engine)
        └── VMM, one process per VM (unprivileged, seccomp, cgroups)
              ├── KVM: memory, vCPUs, in-kernel irqchip + PIT
              ├── devices: 8250 serial, virtio-mmio: blk, vhost-vsock
              │   optional: virtio-net, framebuffer, virtio-input
              └── vsock ──► guest agent (Go, PID 1): exec, files, exit code
```

## Build stages

Status: stages 1-6 done.

1. ✅ **Hello guest** – KVM ioctls, map memory, run a tiny real-mode program,
   catch its port I/O via `KVM_EXIT_IO`.
2. ✅ **Boot Linux** – x86 boot protocol (bzImage, boot_params/zero page, e820,
   cmdline), GDT + page tables, CPUID passthrough, `KVM_CREATE_IRQCHIP`,
   `KVM_CREATE_PIT2`, 8250 UART on 0x3f8 for `console=ttyS0`.
   Boots the host Fedora kernel to userspace in ~0.9 s. Devices: 16550A UART
   (IRQ 4), CMOS RTC, i8042 stub (reboot = VM exit). No PCI/ACPI.
3. ✅ **Agent in initramfs** – static Go agent + busybox, run commands.
   `ksar run -image alpine.tar -- CMD`: Docker export tar -> initramfs,
   agent as PID 1 on COM2 with a credit-flow-controlled framed protocol,
   files in, stdin/stdout/stderr streamed, exit codes like docker (124
   timeout, 125 ksar error, 127 not found, 128+N signal). ~0.8 s boot+run.
   Known limit: serial I/O is ~110-140 KB/s (a VM exit per byte); fixed by
   vsock in stage 4. Images unpack into RAM (size them with -mem).
4. ✅ **virtio-mmio** – virtio-blk for a real rootfs, vhost-vsock for the agent channel.
   Own kernel (Linux 6.18 LTS, tinyconfig + kernel/ksar.config, 2.2 MB,
   userspace in ~0.13 s). virtio-mmio transport, virtio-blk (userspace),
   vhost-vsock (kernel data path; host listens, only our CID accepted).
   Images: Docker tar -> EROFS via mkfs.erofs --tar (cached in
   ~/.cache/ksar/images), mounted read-only under a tmpfs overlay; agent
   switches root and is invisible to the guest. Boot+run ~0.43 s; stdin
   ~50 MB/s+; Alpine/Debian/Ubuntu/Fedora/Arch all run in 256 MB.
   Legacy mode (serial + initramfs) remains for non-Ksar kernels.
5. ✅ **Sandbox hardening** – bounds-check all guest data through one helper,
   seccomp after setup, unprivileged user, cgroups, timeouts, fuzzing.
   - Fuzzing (`make fuzz`): virtqueues, virtio-blk, MMIO transport, UART,
     port bus, agent frames, tar import, kernel loader. Guard region after
     guest RAM catches out-of-bounds writes.
   - Lockdown before the guest runs: seccomp allowlist on every thread
     (TSYNC) with argument checks (KVM_RUN/IRQ_LINE/vhost ioctls only,
     clone = threads only, no PROT_EXEC), no_new_privs, non-dumpable (no
     ptrace or /proc/PID/mem by other processes), no core dumps. Refuses
     root. ksar is static Go (cgo builds refuse lockdown).
   - Cgroups via a systemd user scope: memory = guest + 256 MiB, no swap,
     256 tasks, 150% CPU (auto; falls back without systemd).
   - Terminal output filter (internal/termsafe): on a tty, only text and
     SGR colors pass; OSC (clipboard, title), cursor movement, resets, C1
     controls are removed. Pipes/files untouched; -raw disables.
   - Not done: Landlock (per-thread, can't cover Go's existing threads),
     namespaces (would need a separate VMM process).
6. ✅ **Local web UI** – embedded UI, xterm.js terminal over WebSocket.
   `ksar ui`: server on 127.0.0.1 (Host check, startup token -> HttpOnly
   SameSite=Strict cookie, Origin check on mutations, CSP, no framing).
   Each run is a child `ksar run` (own lockdown + cgroup) reporting JSON
   events on fd 3 and the guest console on fd 4; output is coalesced,
   filtered with termsafe and streamed over SSE with resume. Vanilla JS
   page embedded in the binary, guest output only via textContent.
   Screens: runs, new run, live run (terminal, kernel log, cgroup metrics,
   real protections), images (Docker import, cache), settings (checks).
7. **Optional devices** – networking, then display (framebuffer + VNC/noVNC).
8. **Snapshots** – save/restore memory + vCPU state for fast starts
   (reseed guest RNG after restore).

## Security rules

**Web side (cheap, mandatory):**
- Listen on `127.0.0.1` only.
- Random token generated at startup, required on every request and WebSocket.
- Check the `Origin` header on every request and WebSocket.
- Never run as root; user only needs the `kvm` group.

**VMM side (ongoing discipline):**
- Keep the device set small.
- Never trust guest data: validate every address/length against guest memory.
- Contain the VMM process: unprivileged, seccomp, cgroups.
- Prefer in-kernel implementations (irqchip, vhost-vsock).
- Fuzz device code with `go test -fuzz`.

## Go notes

- `runtime.LockOSThread()` in each vCPU goroutine.
- `golang.org/x/sys/unix` for ioctl/mmap; define KVM structs with exact C layouts.
- Interrupt `KVM_RUN` with a signal + `immediate_exit` in `kvm_run`.
- Careful with `unsafe.Pointer` into mmapped guest memory.

## Guest kernel

`make kernel` builds Linux LTS from `kernel/ksar.config` on top of
tinyconfig: no PCI/ACPI/USB/modules, virtio-mmio + blk + vsock, EROFS,
overlayfs. The build fails if any requested option gets dropped. Version
suffix `-ksar` lets `ksar run` detect it and pick virtio mode.

Next for the kernel: virtio-rng (entropy), and KVM_IOEVENTFD/IRQFD to skip
VMM round trips on queue kicks.

## Gaps found by real-project testing

- Fixed: image environment (PATH etc.) lost by docker export -> NAME.tar.json
  sidecar; the agent now resolves programs with the command's own PATH.
- Fixed: guest kernel created an unused sit0 tunnel device -> IPV6_SIT off.
- Open: copying a whole directory in (-copy and UI uploads are per file;
  today: bake the project into the image, or upload a tar and untar it).
- Open: projects whose `npm test` runs lint hooks needing git; images must
  contain every tool a test script calls (no network to fetch them).

## Scope decisions

- x86_64 only for now.
- Host: Linux with KVM (verified: Fedora 41, Go 1.25, `/dev/kvm` and
  `/dev/vhost-vsock` accessible, Intel VT-x).

## References

- kvmtool (lkvm), gokvm
- Linux `Documentation/virt/kvm/api.rst`, `Documentation/arch/x86/boot.rst`
- virtio 1.x specification
