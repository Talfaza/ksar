# Ksar

Run untrusted code in throwaway KVM microVMs, from your browser. One Go
binary, no root.

```sh
make && make kernel   # once
./ksar                # opens Ksar in your browser
```

Pick an image and a command, or drop in a project folder or archive, and
press Run: a fresh microVM boots in about half a second, runs it, streams
the output to the page, and is destroyed. Nothing written inside survives
the run. Need a screen? Pick a display size and you get a full Linux
desktop (XFCE) in the browser tab, about 4 seconds after you click Run.

Ksar is its own virtual machine monitor: it talks to `/dev/kvm` directly
instead of using QEMU or Firecracker, with a small set of devices written
for this purpose. Everything the page does is also available from an
optional command line (`ksar run`, `ksar ps`, ...).

> *Ksar* (قصر) is a fortified village of southern Morocco: what's inside
> the walls stays inside.

## Contents

- [Requirements](#requirements)
- [Build](#build)
- [Web UI](#web-ui)
- [Command line (optional)](#command-line-optional)
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

## Web UI

```sh
ksar               # serve the UI and open it in your browser
ksar ui            # serve it and print the sign-in link (e.g. over SSH)
```

A local control panel to start runs, watch their output live, type to
their stdin, stop them, and manage images. It looks like this: a runs
list with status and exit codes; a new-run form with secure defaults; a
live run page with a terminal (colors kept), the guest kernel log, cgroup
memory and CPU, and the protections that actually apply to that run; an
images page that imports from Docker; and settings with system checks.

**Run history.** Finished runs, with their output, are kept in
`~/.local/share/ksar/runs` (only your user can read them) and are still
there after Ksar restarts; the latest 200 are kept. Delete one from its
page or clear them all in Settings; `ksar ui -history ""` keeps nothing.

**Desktops, ready ahead of time.** The first display run of an image
installs a minimal XFCE into a cached layer (about a minute for Debian,
longer for Fedora or Arch). **Prepare Desktop** on the Images page does
that in the background, in the same kind of sandboxed install VM, so the
first display run starts in seconds too.

**Run a project.** Click **Run a Project** (or drop a folder, `.zip` or
`.tar.gz` into the new-run form) and Ksar works out what it is from its
files and fills in the run: Python (`requirements.txt`, `pyproject.toml`,
`main.py`, tests), Node (`package.json` start or test script), Go
(`go.mod`), Rust (`Cargo.toml`), Make, C/C++ or a shell script. It picks
the image (importing e.g. `python:3.12-slim` from Docker on first use), a
command that installs dependencies and runs it, and a network allow list
limited to that language's package registry for the install. Projects
with several parts (a Python API and a Node front end, a Go service with
Python scripts, a C program in a subfolder) are searched as a whole, three
folders deep: each part gets its own commands, run from its folder with
its own image, and a top-level `run.sh` or Makefile that drives them gets
one image with every runtime it calls (e.g. `node:22`, which has Python
too) and every registry it needs (or, for Node.js with Go or Rust, a
checksum-pinned toolchain). **Databases come along:** a project whose
manifests use MySQL, PostgreSQL or Redis gets that server installed from
the image's own distribution mirror and started inside the same VM, on
127.0.0.1 only, before its command; when its `.env` or
`docker-compose.yml` names a local database (a DSN, a URL or `DB_*`
settings, also via a compose service name like `db`), that user and
database are created, otherwise `app`/`app`, and `DATABASE_URL` and
friends are exported. Values from the project are only used when they
match a strict character set. Node runs get `CI=true` and `HUSKY=0` (no
watch modes, no git hooks), and a project with git hooks gets git and an
empty repository. A project with several parts runs all of them by
default; the model ranks the alternatives. Review or
edit anything, press Run, and the output streams in. **The command is chosen by a tiny AI model built into Ksar:**
SmolLM2-135M-Instruct, run on the CPU by Ksar's own pure-Go inference
code (no GPU, no Ollama, no cgo; an AVX2 kernel where available). It is
downloaded once (about 260 MB, pinned revision and checked by SHA-256)
from Settings, from the project card, or with `ksar suggest -download`.
It doesn't write commands: Ksar builds a short list of plausible ones
from the project's files (each entry point, script and Makefile target,
with and without the dependency install), and the model picks the most
likely, in about a second. The card shows its pick and every option it
weighed. So it can't produce nonsense, a malicious README can't make it
suggest anything outside that list, and the network policy never comes
from it. Without the model, rules choose. Review the filled-in form and
press Run. From the terminal:

```sh
ksar suggest ./my-project      # how Ksar would run it, and the model's ranking
```

Archives are
unpacked with the same rules as folder uploads: paths must stay inside
the folder, symlinks and special files are skipped, and size and file
limits apply to what they expand to. Nothing from the project runs on
your machine; detection only reads file names and small manifests.

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `127.0.0.1:8765` | Listen address. Loopback IPs only; port 0 picks a free one. |
| `-images DIR` | `~/.local/share/ksar/images` | Directory of root filesystem tars (symlinks allowed). |
| `-kernel FILE` | auto | Kernel for runs. |
| `-open` | off | Open the sign-in link with `xdg-open` (what plain `ksar` does). |
| `-history DIR` | `~/.local/share/ksar/runs` | Where finished runs are kept across restarts; `""` keeps none. |

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

Stopping the UI stops its runs.

## Command line (optional)

Everything below is optional: the web UI starts each run as one of these
`ksar run` processes, and they are there for scripts and terminals.

```
ksar run [flags] [--] COMMAND [ARGS...]
```

| Flag | Default | Meaning |
|---|---|---|
| `-image FILE` | none | Root filesystem as a tar, e.g. from `docker export`. Without it the guest contains only the agent. |
| `-mem SIZE` | `256M` | Guest RAM (e.g. `512M`, `2G`; at most 3 GiB). |
| `-timeout D` | `1m` | Kill the VM after this long (`0` = never). |
| `-i` | off | Forward ksar's stdin to the command. |
| `-copy HOST:GUEST` | | Copy a host file or directory into the guest before running (repeatable). |
| `-e KEY=VALUE` | | Set an environment variable (repeatable). |
| `-w DIR` | `/` | Working directory inside the guest. |
| `-display SIZE` | none | Give the guest a screen, keyboard and mouse: `on` (1280x800) or e.g. `1920x1080`. See [Display](#display). |
| `-desktop` | off | With `-display` (implied): start X and XFCE, then run COMMAND on the desktop (no COMMAND: just the desktop). Images without a desktop get one installed once, see below. |
| `-layer TAR` | | Stack a tar (e.g. from `-save-changes`) read-only on the image (repeatable, topmost first). |
| `-save-changes FILE` | | If COMMAND exits 0, save everything it changed in the filesystem to FILE as a tar, usable with `-layer`. |
| `-vnc ADDR` | `127.0.0.1:0` | With `-display`: where to serve VNC, `127.0.0.1:PORT` (a password is printed) or `unix:PATH` (repeatable). |
| `-net MODE` | `none` | Network access: `none` (no network card), `allow` (only `-allow` hosts) or `internet` (any public address). See [Networking](#networking). |
| `-allow RULE` | | With `-net allow`: a host the guest may reach (repeatable, or comma-separated). |
| `-v` | off | Show the guest kernel log, timings and the cgroup scope. |
| `-raw` | off | Don't filter control sequences in output sent to a terminal. |
| `-cgroup MODE` | `auto` | Per-run cgroup limits via a systemd user scope: `auto`, `on`, `off`. |
| `-kernel FILE` | auto | bzImage to boot. |
| `-legacy` | off | Use the serial channel and an in-RAM image even with the Ksar kernel. |
| `-lockdown` | on | Seccomp lockdown before the guest runs. Disable only to debug. |
| `-agent FILE` | embedded | Use another ksar-agent binary. |
| `-id NAME` | random | This VM's id for `ksar ps` / `ksar stop` (lower-case letters, digits, `-`). |
| `-label TEXT` | | What `ksar ps` shows instead of the command. |
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

# Test a whole project checkout (the directory is copied in; your copy is untouched)
ksar run -mem 1G -image python.tar -copy ./myproject:/work/myproject -w /work/myproject -- python3 -m pytest

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

### Networking

Runs have **no network** unless you ask for one: the guest doesn't even
get a network card. With `-net`, it gets one (`eth0`, 10.0.2.15), and
every packet it sends goes to a separate Ksar process, the network proxy.
The proxy runs its own userspace TCP/IP stack (gVisor's netstack), so the
guest never touches your machine's network directly. Each connection the
guest opens is checked against the policy, then made again by the proxy
as an ordinary connection from your machine. DNS is answered by the proxy
too.

```sh
# Install dependencies from PyPI, and nothing else
ksar run -net allow -allow pypi.org,files.pythonhosted.org \
  -image build/python.tar -- sh -c 'pip install requests && python3 app.py'

# Any public address
ksar run -net internet -image build/alpine.tar -- wget -qO- https://example.com
```

Allow rules: `pypi.org` (that name exactly), `*.pythonhosted.org` (its
subdomains), `github.com:443` (one port), `1.2.3.4`, `10.1.0.0/16:8080`.

What the policy enforces:

- **Names.** In `allow` mode, names off the list don't resolve. A
  connection is allowed only to an address the proxy's own DNS returned for
  an allowed name (or an allowed address), so the guest can't reach an
  address by guessing it or by bringing its own resolver.
- **No local network.** Private, loopback, link-local (including cloud
  metadata at 169.254.169.254), carrier-grade NAT, multicast and reserved
  ranges, and every address of your own machine, are blocked in both
  modes. DNS answers pointing there are dropped (no DNS rebinding). The
  only way through is an explicit address rule, e.g.
  `-allow 192.168.1.10:8080`.
- **What is checked is what is reached.** The proxy connects to exactly
  the address the guest dialed, after checking it, with no second lookup.
- **TCP and DNS only.** Other UDP and ICMP don't leave the proxy (ping to
  outside addresses fails rather than being faked). IPv4 only.
- **Logged.** Blocked lookups and connections are printed to stderr as
  `[ksar] net: …` (all activity with `-v`), and the web UI shows every
  lookup and connection on the run's Network tab.
- **Contained.** The proxy is its own process with its own seccomp filter
  (IP sockets and read-only file opens only: no exec, no file writes, no
  Unix or raw sockets), in the run's cgroup, and dies with the run. The
  VMM stays as locked down as before: it only moves frames over a
  socket pair it opened before lockdown.

Throughput is about 50 MB/s, plenty for installing dependencies.
`internet` mode lets the code reach anything public, including sending
mail or attacking other hosts, so prefer an allow list.

### Display

Runs have no screen unless you ask for one. With `-display`, the guest
gets a framebuffer plus a virtio keyboard and tablet, and you watch and
use it over VNC, or on the run's **Display** tab in the web UI.

The easy way is `-desktop` (what the web UI does when you pick a display
size): it starts Xorg and XFCE and runs your command on that desktop, or
just the desktop if you give no command. **Any Alpine, Debian, Ubuntu,
Fedora or Arch image works**, even one with no X at all:

```sh
ksar run -mem 1536M -timeout 30m -desktop -image build/debian.tar
ksar run -mem 1536M -timeout 30m -desktop -image build/ubuntu.tar -- xfce4-terminal
```

The first time, Ksar installs a minimal XFCE for that image, which takes
a few minutes. The install runs the image's own package manager, so it
happens in a Ksar VM like any run, whose network reaches only that
distribution's package mirrors (e.g. `deb.debian.org`). Only the files
the install changed come back (`-save-changes`), stored as a separate
read-only layer in `~/.cache/ksar/desktop`. Later display runs stack that
layer on the untouched image (`-layer`) and start in under a second.
Deleting the image or clearing the disk cache removes its layer.

To set it up by hand instead, any image with Xorg works;
`examples/desktop` builds a small Alpine one with Xorg, xterm and XFCE:

```sh
docker build -t ksar-desktop examples/desktop
docker create --name d ksar-desktop && docker export d > build/desktop.tar && docker rm d

ksar run -mem 1536M -timeout 30m -display 1280x800 -image build/desktop.tar -- \
  sh -c 'Xorg :0 -nolisten tcp & sleep 2; dbus-launch --exit-with-session startxfce4'
# [ksar] display 1280x800: vnc://127.0.0.1:40123 (password k3m8...)
```

Then open that address in any VNC viewer (e.g. `vncviewer 127.0.0.1::40123`).
With a display, the agent sets `DISPLAY=:0` and writes an Xorg config
for the virtual keyboard and tablet (the guest has no udev to find them).

How it is built, and why it is safe:

- **The screen is plain memory.** The kernel is told about a linear
  framebuffer (as firmware would) and drives it with `simpledrm`. Ksar
  only reads that memory; there is no guest display protocol to parse.
- **Input is one way.** The keyboard and tablet are virtio-input devices
  that Ksar only writes events to. What the guest sends back (keyboard
  LEDs) is discarded.
- **No clipboard.** Clipboard text from the viewer is dropped, and
  nothing in the guest can read your clipboard.
- **Viewers are checked.** VNC is served on loopback only, with a random
  password over TCP; a `unix:` socket is created mode 0600. The web UI
  uses a private socket and reaches it only through its signed-in session
  (with an Origin check on the WebSocket). Every viewer message is size
  checked, and the server is fuzzed.
- **The lockdown is unchanged** except that the VMM may now read its own
  socket address after accepting a viewer, and set `TCP_NODELAY`, the one
  socket option Go sets on accepted TCP connections.

Images are converted to EROFS disks on first use and cached in
`~/.cache/ksar/images` (keyed on the tar's path, size and mtime). Delete
that directory to reclaim space.

**Image settings.** `docker export` keeps only the filesystem, not the
image's environment (for example the `PATH` where a Go, Java or Rust
image installs its toolchain) or working directory. Ksar reads them from
an optional file next to the tar, `NAME.tar.json`:

```json
{ "env": ["PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin", "GOPATH=/go"], "workdir": "/src" }
```

The web UI's Docker import writes it automatically. To make one by hand:
`docker inspect IMAGE --format '{{json .Config}}'` has the values. `-e`
and `-w` override what the file sets.

**Copying directories.** `-copy DIR:/guest/path` (and "Add folder" in the
web UI) copies a directory tree, with permissions and empty directories,
as one stream. The directory is treated as untrusted: symlinks are copied
as links and never followed, so a link like `notes -> ~/.ssh/id_rsa`
can't pull your key into the guest; device files, sockets and pipes are
skipped. Browsers don't send permissions, so in the web UI files starting
with `#!` are made executable and the rest are not.

**Writable space.** Writes go to a RAM-backed layer of up to half the
guest RAM (about 120 MB with the default 256 MB). Give more `-mem` to
commands that write a lot.

Tested with Alpine, Debian, Ubuntu, Fedora and Arch Linux images, all in
the default 256 MiB.

### VM ids: `ksar ps` and `ksar stop`

Every VM has an id: random 12 characters, or your own with `-id NAME`.
`-label TEXT` sets what `ksar ps` shows instead of the command. Runs
started from the web UI use the UI's run id, so both show the same one.

```
$ ksar ps
ID            STATUS   UPTIME  MEMORY   IMAGE   COMMAND
34afe4c64e54  running  2m05s   1.1 GB   debian  desktop  [display]
web-test      running  14s     51.0 MB  alpine  python3 app.py  [net: allow]
$ ksar stop 34af web-test     # any unique prefix works
```

`ksar ps -a` also shows VMs that exited in the last hour, with their exit
codes; `-q` prints only ids and `-json` everything. The id is also the
cgroup scope's name (`ksar-<id>-….scope`) and is visible inside the guest
in `/run/ksar/id`. `ksar stop` sends SIGTERM (the VM ends with 143), then
SIGKILL after `-t` (5 s).

The list lives in `$XDG_RUNTIME_DIR/ksar/vms`. A running ksar is locked
down and may not create or delete files, so it opens its entry before
lockdown and only rewrites it in place afterwards; entries of processes
that are gone (or whose PID now belongs to someone else) are cleaned up
by `ksar ps`.

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
 │   ├── virtio-net (-net)     frames over a socket pair to the network proxy
 │   ├── framebuffer (-display) guest memory the VMM reads; served over VNC
 │   ├── virtio-input ×2        keyboard and tablet, events from the viewer
 │   └── CMOS clock, reset line
 ├── session ◄──── vsock ────► ksar-agent (PID 1 in the guest)
 │                             mounts the image under a RAM overlay,
 │                             runs the command, streams its I/O
 └── ksar net-proxy (only with -net; its own process and seccomp filter)
     userspace TCP/IP (gVisor netstack), policy, DNS, logging
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
blk, vsock, net, input, simpledrm, EROFS, overlayfs, ext4, tmpfs, namespaces, seccomp. No modules,
PCI, ACPI, USB or graphics. It is 2.5 MB and reaches userspace in ~0.13 s.
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
the timeout. By default it has no network: there is no network device at
all. With `-net` it reaches only what the policy allows, through the
proxy (see [Networking](#networking)).

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

## Tested with real projects

Each ran its own test suite inside a VM with no network, dependencies
baked into the image, four VMs at once:

| Project | Command | Result |
|---|---|---|
| [more-itertools](https://github.com/more-itertools/more-itertools) (Python) | `python3 -m pytest` | 765 tests + 21,194 subtests passed, 20 s, 1 GB |
| [node-semver](https://github.com/npm/node-semver) (Node.js) | `npm test --ignore-scripts` (tap) | passed, 100% coverage, 38 s, 1.5 GB |
| [google/uuid](https://github.com/google/uuid) (Go) | `go test ./...` (compiled in the VM) | passed, 17 s, 1 GB |
| [cJSON](https://github.com/DaveGamble/cJSON) (C) | cmake build + `ctest` | 19/19 passed, 8 s, 1 GB |

And hostile workloads, all contained with the host unaffected: reaching
the internet (no network interface but `lo`), finding host files or SSH
keys, writing to the image disk (read-only), a fork bomb and an infinite
loop (killed at the timeout), holding 1.5 GB in a 256 MB guest (the guest's
OOM killer stops it), filling the disk (only the RAM layer fills), loading
kernel modules or triggering sysrq (not supported by the guest kernel),
and terminal escape injection (filtered).

## Performance

On a 16-core laptop (Intel, Fedora 41), with the Ksar kernel:

| | Time |
|---|---|
| `ksar run -- true` (boot, run, teardown) | ~0.2–0.5 s |
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
| `internal/virtio` | virtio-mmio transport, virtqueues, virtio-blk, virtio-net, virtio-input, vhost-vsock |
| `internal/display` | VNC (RFB 3.8) server over the guest framebuffer, keyboard map |
| `internal/netproxy` | Network proxy: netstack, policy, DNS filtering, connection log |
| `internal/agent` | Host/guest protocol and client |
| `internal/sandbox` | One run: initramfs, boot, session, teardown |
| `internal/image` | Docker tar to EROFS, cache |
| `internal/initramfs` | cpio (newc) writer, tar import |
| `internal/harden` | Lockdown: seccomp (VMM and network proxy filters), no_new_privs, non-dumpable |
| `internal/termsafe` | Terminal control-sequence filter |
| `internal/ui` | Web UI server (security checks, run supervisor, SSE streaming) and its embedded page |
| `kernel/` | Guest kernel config and build script |
| `PLAN.md` | Design notes and stage-by-stage history |

## Status and roadmap

Done: KVM VMM, Linux boot, guest agent, own kernel with virtio
(blk + vsock + net + input), EROFS images, hardening, local web UI,
optional networking (allow list or internet) and display (VNC and in the
web UI), both off by default.

Next:

- Snapshots for faster starts (built once and shelved, see PLAN.md)
- Multiple vCPUs, image commands (`ksar pull`, `ksar images`), ioeventfd
