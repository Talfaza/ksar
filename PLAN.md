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
| Network        | **None** (no network card)                | Allow list of hosts, or the internet (public addresses), via a userspace proxy |
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

Status: stages 1-7 done; stage 8 (snapshots) shelved.

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
6. ✅ **Local web UI** – embedded UI with its own terminal renderer over SSE.
   `ksar ui`: server on 127.0.0.1 (Host check, startup token -> HttpOnly
   SameSite=Strict cookie, Origin check on mutations, CSP, no framing).
   Each run is a child `ksar run` (own lockdown + cgroup) reporting JSON
   events on fd 3 and the guest console on fd 4; output is coalesced,
   filtered with termsafe and streamed over SSE with resume. Vanilla JS
   page embedded in the binary, guest output only via textContent.
   Screens: runs, new run, live run (terminal, kernel log, cgroup metrics,
   real protections), images (Docker import, cache), settings (checks).
7. ✅ **Optional devices** – networking and display.
   Networking: virtio-net in the VMM moves frames over a SOCK_SEQPACKET
   socket pair to `ksar net-proxy`, a separate process (own seccomp
   filter, same cgroup, Pdeathsig) running gVisor netstack in promiscuous
   mode. TCP is terminated there and re-dialed from the host after the
   policy check; DNS (10.0.2.3) is answered there, filtered by name, with
   private answers dropped. `allow` mode only permits addresses its own
   DNS returned for allowed names. Private/local/metadata ranges and the
   host's own addresses are blocked unless named by an address rule. The
   guest is configured by the kernel (`ip=`, CONFIG_IP_PNP); the agent
   copies /proc/net/pnp to /etc/resolv.conf. gVisor is pinned at
   ace9117eb232 (newer snapshots need Go 1.27 or carry a broken test file).
   Display: a linear framebuffer in a second KVM memory slot at 3 GiB
   (above the largest RAM, below virtio-mmio), announced in boot_params'
   screen_info as a VESA LFB (xRGB8888) and driven by simpledrm (sysfb),
   with fbdev emulation for /dev/fb0 and a VT for Xorg. Keyboard and
   tablet are virtio-input (host -> guest events only; the status queue
   is drained and ignored; config space is select/subsel driven, the one
   writable config space). internal/display serves RFB 3.8 from the
   framebuffer: Raw encoding, any true-color pixel format, tile-diffed
   against a per-viewer shadow copy, VNC auth over TCP, clipboard
   dropped. Listeners open before lockdown; the VMM filter gained
   getsockname and setsockopt(TCP_NODELAY) only. The agent writes an
   xorg.conf.d snippet naming the event devices (no udev) and defaults
   DISPLAY=:0. The web UI bridges a WebSocket (Origin checked) to a
   private Unix socket per run and draws with its own RFB client
   (vnc.js). Tested with xterm/twm and XFCE on Alpine
   (examples/desktop).
   Desktops for any image (`-desktop`, the UI's default for display
   runs): internal/desktop inspects the tar (os-release, Xorg, XFCE) and,
   if needed, installs a minimal XFCE with the distro's package manager
   (Alpine, Debian, Ubuntu, Fedora, Arch) in a Ksar VM whose network
   reaches only that distro's mirrors. The agent exports the overlay's
   upper dir (os.Root opened before switch_root; whiteouts and the
   opaque/capability xattrs kept) as a tar (`-save-changes`, protocol
   Export/ExportData); it is cached per image and stacked read-only at
   run time (`-layer`, agent `ksar.layers=`, multi-lower overlay). The
   tmpfs upper may now use 85% of RAM.
   VM ids: every ksar run has an id (random or -id), registered in
   $XDG_RUNTIME_DIR/ksar/vms/<id>.json (internal/registry). The entry is
   opened before lockdown and rewritten in place with pwrite (fixed
   8 KiB, space padded), since lockdown forbids creating, truncating or
   deleting files; readers drop entries whose PID is gone or reused
   (start time in /proc/PID/stat). ksar ps / ksar stop (prefix match,
   SIGTERM then SIGKILL); SIGTERM/SIGINT are caught to record the exit.
   The id names the cgroup scope, is passed through the scope re-exec,
   and reaches the guest as /run/ksar/id. The UI passes its run id.
8. **Snapshots** – shelved. A working version (boot ~520 ms -> ~20 ms for
   runs without network or display: port-I/O snapshot point in the agent,
   full vCPU/VM/device/RAM save under lockdown, copy-on-write restore,
   vsock transport reset, TSC/kvmclock catch-up, RNG reseed) was built on
   2026-10-22 and removed the same day as not worth its weight yet. The
   code is archived in build/attic/snapshots-feature-2026-10-22.tar.gz.
   Kept from it: the Hello message (VM id + entropy reseed at the start of
   every run).

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
- Fixed: copying a whole directory in: -copy DIR and UI "Add folder" stream a
  tar (TarOpen/Data/Close) packed before lockdown into an unlinked temp
  file; symlinks never followed, special files skipped.
- Open: projects whose `npm test` runs lint hooks needing git.
- Fixed: images had to contain every tool a test script calls -> optional
  networking, e.g. `-net allow -allow pypi.org,files.pythonhosted.org`.

## Scope decisions

- x86_64 only for now.
- Host: Linux with KVM (verified: Fedora 41, Go 1.25, `/dev/kvm` and
  `/dev/vhost-vsock` accessible, Intel VT-x).

## References

- kvmtool (lkvm), gokvm
- Linux `Documentation/virt/kvm/api.rst`, `Documentation/arch/x86/boot.rst`
- virtio 1.x specification
