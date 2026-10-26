# Security policy

Ksar runs code you don't trust, so its security bugs matter more than most.
Thank you for helping find them.

Ksar is **alpha** software and has not had a security audit. The threat model
and the protections it relies on are described in the README's
[Security](README.md#security) section.

## Reporting a vulnerability

**Please don't open a public issue for a security problem.** Report it
privately instead:

1. Go to the repository's **Security** tab.
2. Click **Report a vulnerability**
   ([direct link](https://github.com/Talfaza/ksar/security/advisories/new)).
3. Describe the problem, how to reproduce it (a minimal image, command,
   project or page), the Ksar commit you tested, your host kernel and
   distribution, and the impact you think it has.

A proof of concept helps a lot, but please keep it to what's needed to show
the problem.

## What to expect

Ksar is maintained by one person, so these are goals, not guarantees:

- An acknowledgement within **7 days**.
- A first assessment (whether it's in scope, and how severe) within **14 days**.
- A fix, or a plan, as soon as practical; serious issues come first.
- Coordinated disclosure: we agree on a date to publish, normally once a fix
  is available and at most **90 days** after the report. You will be credited
  in the advisory unless you'd rather not be.

## Supported versions

There are no releases yet. Only the latest commit on `master` is supported;
please check that a problem still exists there before reporting it.

## Scope

In scope, for example:

- **Escaping the VM:** guest code affecting the host through Ksar's devices
  (virtio, serial, framebuffer, input), the boot path or the agent protocol.
- **Weakening the lockdown:** getting a locked-down `ksar` process or the
  network proxy to open files, create sockets, run programs or otherwise
  do what its seccomp filter forbids.
- **Bypassing the network policy:** reaching hosts outside an allow list,
  your local network, your machine's own addresses or cloud metadata
  endpoints, or getting a network when a run has none.
- **The web UI:** acting on it from another website or local port (CSRF, DNS
  rebinding, origin checks), using it without the sign-in link, script
  injection through guest output, or viewing another run's screen without
  the session.
- **Uploads and projects:** archive extraction writing outside its folder or
  following links; project files (manifests, `.env`, `docker-compose.yml`,
  READMEs) getting commands or values into a run that Ksar didn't build and
  check, including through the built-in model.
- **Isolation between runs:** one run seeing or changing what another wrote.
- **Terminal output:** control sequences reaching your terminal or the page
  despite the filter.
- **Integrity of downloads:** the model or a runtime Ksar installs (Go,
  Node.js) being used without its checksum being verified.

Out of scope:

- What a run is allowed to do by design: use its CPU, memory and time up to
  its limits; reach the public internet in **Internet** mode, or the hosts on
  its allow list.
- Setups that turn protections off: `-lockdown=false`, `KSAR_SECCOMP_DEBUG`,
  `-allow-root`, `-cgroup off`, `-raw`, or a UI listening on a non-loopback
  address through a proxy you set up.
- Bugs in the host kernel, KVM or vhost-vsock themselves (please report those
  to the Linux kernel security team), in Docker, or in the images you run,
  unless Ksar makes them reachable in a way it shouldn't.
- Attacks that need control of your user account, your browser or your
  machine already.
- Secrets you put into a run yourself (environment variables, files, a
  project's `.env`), and which therefore appear in that run's output or
  history.

## Safe harbor

Research done in good faith under this policy, on your own machines and
without harming others or their data, is welcome. We won't pursue legal
action over it, and we'll work with you to understand and fix the problem.
