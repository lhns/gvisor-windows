# gvisor-windows

gVisor is an application kernel for containers: it implements the Linux system-call interface in user space and runs OCI containers through its runtime, `runsc`. **This unofficial fork adds Windows as a host**, so `runsc` and the Sentry run on Windows for a standard user, without admin rights, Hyper-V, WSL or a VM. Not a Google project.

Status: `runsc.exe run` boots containers on Windows; Podman acceptance in progress.

**Not a security sandbox on Windows.** The port runs Linux code natively and catches its system calls by rewriting them; code that jumps into the middle of an instruction can still reach the Windows kernel, and host folders are opened with the user's own rights. Treat it as a compatibility layer, not an isolation boundary. Roadmap: [docs/roadmap.md](docs/roadmap.md). Decisions: [docs/adr/](docs/adr/).

---

Upstream README of the `go` branch:

# gVisor

This branch is a synthetic branch, containing Go sources and the C sources of
runsc's sidecars, that is compatible with standard Go tools. Build runsc and
its Go sidecars with CGO_ENABLED=0, and build the C sidecars with the flags in
their BUILD files on the master commit that each go branch commit merges. See
the master branch for authoritative sources and tests.
