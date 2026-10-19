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

Status: stages 1-2 done.

1. ✅ **Hello guest** – KVM ioctls, map memory, run a tiny real-mode program,
   catch its port I/O via `KVM_EXIT_IO`.
2. ✅ **Boot Linux** – x86 boot protocol (bzImage, boot_params/zero page, e820,
   cmdline), GDT + page tables, CPUID passthrough, `KVM_CREATE_IRQCHIP`,
   `KVM_CREATE_PIT2`, 8250 UART on 0x3f8 for `console=ttyS0`.
   Boots the host Fedora kernel to userspace in ~0.9 s. Devices: 16550A UART
   (IRQ 4), CMOS RTC, i8042 stub (reboot = VM exit). No PCI/ACPI.
3. **Agent in initramfs** – static Go agent + busybox, run commands.
4. **virtio-mmio** – virtio-blk for a real rootfs, vhost-vsock for the agent channel.
5. **Sandbox hardening** – bounds-check all guest data through one helper,
   seccomp after setup, unprivileged user, cgroups, timeouts, fuzzing.
6. **Local web UI** – embedded UI, xterm.js terminal over WebSocket.
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

The host Fedora kernel boots fine for stages 1-3, but it builds
`virtio-mmio` and vsock as modules. Stage 4 needs our own minimal kernel
config with these built in (also: fewer drivers = smaller attack surface
inside the guest and faster boot).

## Scope decisions

- x86_64 only for now.
- Host: Linux with KVM (verified: Fedora 41, Go 1.25, `/dev/kvm` and
  `/dev/vhost-vsock` accessible, Intel VT-x).

## References

- kvmtool (lkvm), gokvm
- Linux `Documentation/virt/kvm/api.rst`, `Documentation/arch/x86/boot.rst`
- virtio 1.x specification
