# Ksar

Run untrusted code in throwaway KVM microVMs, from one Go binary and
without root.

```console
$ ksar run -image build/alpine.tar -- sh -c 'echo "hello from $(cat /etc/alpine-release)"; exit 3'
hello from 3.20.10
$ echo $?
3
```

Each run boots a fresh microVM (about 0.2 s, boot to teardown), runs the command inside a
Docker image's filesystem, streams stdin/stdout/stderr, returns the exit
code, and destroys the VM. Nothing written inside survives the run.

Ksar is its own virtual machine monitor: it talks to `/dev/kvm` directly
instead of using QEMU or Firecracker, with a small set of devices written
for this purpose.

> *Ksar* (قصر) is a fortified village of southern Morocco: what's inside
> the walls stays inside.

## Contents

- [Requirements](#requirements)
- [Build](#build)
- [Usage](#usage)
- [Web UI](#web-ui)
- [How it works](#how-it-works)
- [Security model](#security-model)
- [Performance](#performance)
- [Development](#development)
- [Status and roadmap](#status-and-roadmap)

## Requirements

- Linux on x86_64 with KVM: `/dev/kvm` readable and writable by your user
  (usually via the `kvm` group: `sudo usermod -aG kvm $USER`, then log in
  again)
- `/dev/vhost-vsock` (module `vhost_vsock`) for the fast path
- Go 1.25 or newer
- `mkfs.erofs` from erofs-utils, to turn images into disks
  (`dnf install erofs-utils` / `apt install erofs-utils`)
- To build the guest kernel: `gcc make flex bison bc perl curl xz` and the
  libelf headers (`elfutils-libelf-devel` / `libelf-dev`)
- Optional: Docker, to export images (`make alpine`)
- Optional: a systemd user session, for per-run cgroup limits

Ksar never needs root, and refuses to run as root unless told to.

## Build

```sh
make            # ksar (with the guest agent embedded), plus a test initramfs
make kernel     # the Ksar guest kernel -> build/bzImage (downloads Linux LTS, ~1-2 min on 16 cores)
make alpine     # export alpine:3.20 to build/alpine.tar (needs Docker)
```

`ksar` is a static, pure-Go binary (`CGO_ENABLED=0`, required by the
seccomp lockdown). The guest agent is embedded in it, so `ksar` plus a
kernel is all you need to deploy.

`ksar run` uses the Ksar kernel automatically once it's built (found at
`build/bzImage` next to the binary, `~/.local/share/ksar/bzImage`, or
`$KSAR_KERNEL`). Without it, Ksar falls back to the host kernel in legacy
mode (see below).

## Usage

```
ksar run [flags] [--] COMMAND [ARGS...]
```

| Flag | Default | Meaning |
|---|---|---|
| `-image FILE` | none | Root filesystem as a tar, e.g. from `docker export`. Without it the guest contains only the agent. |
| `-mem SIZE` | `256M` | Guest RAM (e.g. `512M`, `2G`; at most 3 GiB). |
| `-timeout D` | `1m` | Kill the VM after this long (`0` = never). |
| `-i` | off | Forward ksar's stdin to the command. |
| `-copy HOST:GUEST` | | Copy a host file into the guest before running (repeatable). |
| `-e KEY=VALUE` | | Set an environment variable (repeatable). |
| `-w DIR` | `/` | Working directory inside the guest. |
| `-v` | off | Show the guest kernel log, timings and the cgroup scope. |
| `-raw` | off | Don't filter control sequences in output sent to a terminal. |
| `-cgroup MODE` | `auto` | Per-run cgroup limits via a systemd user scope: `auto`, `on`, `off`. |
| `-kernel FILE` | auto | bzImage to boot. |
| `-legacy` | off | Use the serial channel and an in-RAM image even with the Ksar kernel. |
| `-lockdown` | on | Seccomp lockdown before the guest runs. Disable only to debug. |
| `-agent FILE` | embedded | Use another ksar-agent binary. |
| `-allow-root` | off | Allow running as root (not recommended). |

### Exit codes

Like `docker run`: the command's own exit code, or

| Code | Meaning |
|---|---|
| 124 | The VM hit `-timeout` and was killed |
| 125 | Ksar itself failed (bad flags, boot failure, ...) |
| 126 | The command could not be executed |
| 127 | The command was not found |
| 128+N | The command was killed by signal N (e.g. 137 = SIGKILL) |

### Examples

```sh
# Run a script from the host
ksar run -image build/alpine.tar -copy ./test.sh:/work/test.sh -w /work -- sh test.sh

# Pipe data through a sandboxed command
sort -R names.txt | ksar run -i -image build/alpine.tar -- sort -u

# Language runtimes come from their images
docker create --name tmp python:3.12-slim && docker export tmp -o python.tar && docker rm tmp
ksar run -mem 512M -image python.tar -copy ./script.py:/work/script.py -- python3 /work/script.py

# Any distro Docker can export
docker create --name tmp fedora:41 && docker export tmp -o fedora.tar && docker rm tmp
ksar run -image fedora.tar -- dnf --version

# More memory, longer timeout, see what happens inside
ksar run -v -mem 1G -timeout 5m -image build/alpine.tar -- sh -c 'apk info | wc -l'
```

Images are converted to EROFS disks on first use and cached in
`~/.cache/ksar/images` (keyed on the tar's path, size and mtime). Delete
that directory to reclaim space.

Tested with Alpine, Debian, Ubuntu, Fedora and Arch Linux images, all in
the default 256 MiB.

## Web UI

```sh
ksar ui            # then open the link it prints
ksar ui -open      # or let it open the browser
```

A local control panel to start runs, watch their output live, type to
their stdin, stop them, and manage images. It looks like this: a runs
list with status and exit codes; a new-run form with secure defaults; a
live run page with a terminal (colors kept), the guest kernel log, cgroup
memory and CPU, and the protections that actually apply to that run; an
images page that imports from Docker; and settings with system checks.

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `127.0.0.1:8765` | Listen address. Loopback IPs only; port 0 picks a free one. |
| `-images DIR` | `~/.local/share/ksar/images` | Directory of root filesystem tars (symlinks allowed). |
| `-kernel FILE` | auto | Kernel for runs. |
| `-open` | off | Open the sign-in link with `xdg-open`. |

How it's kept safe:

- **Each run is its own `ksar run` process**, with the same seccomp
  lockdown and cgroup scope as on the command line. The UI server never
  runs guest code.
- **Loopback only**, and the `Host` header must match exactly (stops DNS
  rebinding).
- **Startup token:** the printed link sets an `HttpOnly`,
  `SameSite=Strict` session cookie and redirects to a clean URL. Restart
  `ksar ui` for a new one.
- **Origin checks** on every state-changing request, so pages from other
  websites, or from other servers on your machine (other ports), can't
  start or stop runs.
- **Strict Content-Security-Policy**, no framing, nothing loaded from the
  internet. Guest output is filtered like terminal output, then inserted
  only as text, never as HTML.

Run history lives in memory while `ksar ui` runs. Stopping the UI stops
its runs.

### Other commands

`ksar boot` boots a kernel with an initramfs and shows its console, and
`ksar initramfs` packs a static binary into an initramfs. `ksar hello`
runs a tiny hand-assembled real-mode guest. These are the building blocks
from earlier development stages, kept for debugging.

## How it works

```
 ksar (one static Go process, per run)
 ├── CLI: flags, image cache, cgroup scope, lockdown
 ├── VMM ─────────── /dev/kvm: 1 vCPU, guest RAM, in-kernel irqchip + PIT
 │   ├── 8250 UART (COM1)      guest kernel console
 │   ├── virtio-blk            the image, read-only (/dev/vda)
 │   ├── vhost-vsock           agent channel; data path in the host kernel
 │   └── CMOS clock, reset line
 └── session ◄──── vsock ────► ksar-agent (PID 1 in the guest)
                               mounts the image under a RAM overlay,
                               runs the command, streams its I/O
```

1. **Image.** A Docker export tar is converted once with `mkfs.erofs --tar`,
   which keeps ownership and modes without root.
2. **Boot.** Ksar loads the kernel with the x86 64-bit boot protocol (no
   BIOS), sets up long mode and page tables itself, and starts the vCPU.
   Devices are virtio-mmio, announced on the kernel command line. There is
   no PCI or ACPI.
3. **Root switch.** The agent is the initramfs's only program. It mounts
   the image read-only, stacks a tmpfs on it with overlayfs and switches
   root into it. The agent binary is then invisible to the guest, and
   writes only ever reach RAM.
4. **Session.** The agent connects to the host over vsock and speaks a
   small framed protocol: file transfers, exec, stdin/stdout/stderr, exit
   status. Host-to-guest traffic is credit flow controlled.
5. **Teardown.** When the exit status arrives, Ksar stops the vCPU and
   destroys the VM.

**The guest kernel** (`kernel/ksar.config`, applied on top of
`tinyconfig`) is Linux LTS with only what a sandbox needs: virtio-mmio,
blk, vsock, EROFS, overlayfs, ext4, tmpfs, namespaces, seccomp. No modules,
PCI, ACPI, USB or graphics. It is 2.2 MB and reaches userspace in ~0.13 s.
The build fails if any requested option gets silently dropped.

**Legacy mode** works with any bzImage (e.g. the host's `/boot/vmlinuz-*`):
the image is unpacked into the initramfs (RAM) and the agent talks over a
second serial port. It's slower (~130 KB/s I/O) and needs about 2.5x the
image size in RAM, but needs no kernel build.

## Security model

**Threat:** the code inside the VM is hostile and has root in the guest. It
controls the guest kernel, so it can send anything to Ksar's devices:
malformed virtqueues, bogus register writes, oversized or corrupt protocol
frames, hostile terminal output.

**Goal:** it can't affect the host beyond the resources it was given, and a
bug in Ksar's device code doesn't hand it your machine.

Layers, from the inside out:

1. **Hardware virtualization.** The guest runs under KVM with its own
   kernel. Its only ways out are the devices Ksar emulates and vhost-vsock.
2. **Small, validated device code.** Few devices; every guest-controlled
   value (ring indices, descriptor addresses and lengths, chain links,
   register writes, frame sizes) is checked before use, and all guest
   memory access goes through one bounds-checked function. Queues that
   misbehave are shut down, not trusted. All of this is fuzzed
   (`make fuzz`), with a guard region past guest RAM to catch
   out-of-bounds writes.
3. **Lockdown.** After the VM is built and before the guest runs, Ksar
   restricts itself, on every thread:
   - a seccomp allowlist: no opening files, no sockets, no exec, no new
     processes, no executable memory; ioctls limited to `KVM_RUN`,
     `KVM_IRQ_LINE` and vhost
   - `no_new_privs`, and non-dumpable: other processes of your user can't
     ptrace Ksar or read guest memory through `/proc/PID/mem`
   - no core dumps (they would contain guest memory)
4. **Cgroups.** Each run gets a systemd user scope: memory capped at guest
   RAM plus headroom, no swap (guest memory never reaches disk), at most
   256 tasks and 1.5 CPUs.
5. **Unprivileged.** Ksar needs only `/dev/kvm` and `/dev/vhost-vsock`,
   and refuses to run as root.
6. **The agent channel.** Ksar listens on vsock and the agent connects out.
   Only a connection from the VM's own CID, which the kernel assigns and
   nobody can forge, is accepted. Other local processes and other VMs can't
   pose as the agent.
7. **Terminal output.** Output headed for a terminal is filtered: colors
   pass, but clipboard writes (OSC 52), title changes, cursor movement,
   terminal resets and other control sequences are removed. Pipes and
   files get the exact bytes. `-raw` disables this.

**Isolation between runs:** the image disk is read-only and every run gets
a fresh RAM overlay, so nothing a run writes is visible to the next.

**What the guest can do:** use its CPU and RAM up to the limits, until
the timeout. It has no network: there is no network device at all.

**Known limits:**

- The VMM is one process. A separate jailed VMM process (user, mount and
  network namespaces, like Firecracker's jailer) would add a layer.
  Landlock isn't used because it restricts only the calling thread, and Go
  already has many threads when setup ends; seccomp already forbids
  opening files and sockets.
- vhost-vsock runs in the host kernel, so its bugs are kernel bugs. This is
  the trade-off for its speed.
- Output sent to pipes and files is not filtered, by design.
- No security audit has been done. Treat Ksar as defense in depth, not a
  guarantee.

## Performance

On a 16-core laptop (Intel, Fedora 41), with the Ksar kernel:

| | Time |
|---|---|
| `ksar run -- true` (boot, run, teardown) | ~0.2 s |
| 4 MB through stdin, checksummed | ~0.24 s total |
| 50 MB through stdin, checksummed | ~0.6 s total |
| Arch Linux image, cached | ~0.5 s |
| First use of a 400 MB image (EROFS conversion) | +0.9 s |

## Development

```sh
make test               # unit + end-to-end tests (needs KVM; uses build/bzImage
                        # and build/alpine.tar when present)
go test -race ./...     # race detector (lockdown probes skip: -race needs cgo)
make fuzz FUZZTIME=60s  # fuzz every parser of untrusted input
```

The end-to-end tests run every scenario in both virtio and legacy mode, and
inspect a running ksar from outside to check that the lockdown is in force
on every thread.

Debugging a lockdown failure: `KSAR_SECCOMP_DEBUG=1` makes blocked syscalls
fail with errno 1000+N (N = syscall number) instead of killing ksar. Never
use it with untrusted guests.

### Layout

| Path | What |
|---|---|
| `cmd/ksar` | CLI: `run`, `ui`, `boot`, `initramfs`, `hello`; cgroup scope; terminal filtering; JSON events for the UI |
| `cmd/ksar-agent` | Guest agent, PID 1 in the VM |
| `internal/kvm` | Thin `/dev/kvm` wrapper; struct layouts pinned by tests |
| `internal/boot` | x86 Linux boot protocol, long mode, CPUID |
| `internal/vmm` | VM lifecycle, exit loop, MMIO/port dispatch, agent channels |
| `internal/devices` | 8250 UART, CMOS clock, i8042 reset, port bus |
| `internal/virtio` | virtio-mmio transport, virtqueues, virtio-blk, vhost-vsock |
| `internal/agent` | Host/guest protocol and client |
| `internal/sandbox` | One run: initramfs, boot, session, teardown |
| `internal/image` | Docker tar to EROFS, cache |
| `internal/initramfs` | cpio (newc) writer, tar import |
| `internal/harden` | Lockdown: seccomp, no_new_privs, non-dumpable |
| `internal/termsafe` | Terminal control-sequence filter |
| `internal/ui` | Web UI server (security checks, run supervisor, SSE streaming) and its embedded page |
| `kernel/` | Guest kernel config and build script |
| `PLAN.md` | Design notes and stage-by-stage history |

## Status and roadmap

Done: KVM VMM, Linux boot, guest agent, own kernel with virtio
(blk + vsock), EROFS images, hardening, local web UI.

Next:

- Optional networking: off by default, NAT with an egress allowlist
- Optional display (framebuffer + VNC)
- Snapshots for faster starts
- Multiple vCPUs, image commands (`ksar pull`, `ksar images`), ioeventfd
