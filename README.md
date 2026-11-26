# gvisor-windows

**Unofficial** port of [gVisor](https://gvisor.dev) to Windows hosts: Linux containers (Podman) for a standard Windows user, without admin, Hyper-V, WSL or a VM. Not a Google project.

Status: Phase 0 (feasibility gates). Roadmap: [docs/roadmap.md](docs/roadmap.md). Decisions: [docs/adr/](docs/adr/).

---

Upstream README of the `go` branch:

# gVisor

This branch is a synthetic branch, containing Go sources and the C sources of
runsc's sidecars, that is compatible with standard Go tools. Build runsc and
its Go sidecars with CGO_ENABLED=0, and build the C sidecars with the flags in
their BUILD files on the master commit that each go branch commit merges. See
the master branch for authoritative sources and tests.
