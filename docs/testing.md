# Testing drydock

drydock's own safety model says "do not run the agent's changes on the host".
That applies to developing drydock too: the agent should be able to test it
autonomously without getting host code execution. This document describes where
each kind of test may run.

## Tiers

| Tier | What it covers | Where it runs | Agent code allowed? |
| --- | --- | --- | --- |
| 0. Unit | pure logic: wrapper generation, override parsing, PPM decode, path map | anywhere (`nix flake check`, `go test ./...`) | yes (no host side effects) |
| 1. Integration | full lifecycle against the bundled `demo` | a disposable VM with nested KVM | yes (contained in the VM) |
| 2. Host environment | KVM, store, systemd, noVNC on the real host | the host, driven through the portal | **no** — vetted drydock only |
| 3. Host-fidelity dev | new drydock code against the real host kernel/KVM/store | a dev VM the host boots with nested KVM; never the host itself | yes (contained in the VM) |

The rule of thumb: **the host runs only vetted code; agent-authored code runs in
a VM.**

## Tiers 0 and 1 (autonomous, always safe)

Tier 0 runs without KVM or a host:

```sh
nix flake check                 # builds and runs the unit tests
go test ./...                   # same, from a dev shell
```

Tier 1 exercises the whole thing inside the disposable agent VM:

```sh
# Build and validate the flake/override/wrapper path without booting:
drydock up --flake ./demo --host demo --dry-run
drydock apply --dry-run

# Full lifecycle:
drydock up --flake ./demo --host demo
drydock screenshot /tmp/dd.png
drydock apply
drydock reset --flake ./demo --host demo
drydock down
```

`--dry-run` builds the VM or system derivation and stops before touching KVM or
the running guest, so a wrapper/override change can be validated cheaply.

## Why new drydock code must not run on the host

The portal already executes `nix build` and QEMU on the host. If the portal also
ran an agent-authored `drydock` binary, that binary would run with the portal's
host privileges — the exact failure mode drydock exists to prevent. Containment
options (systemd sandboxing, restricted users, resource limits) reduce the blast
radius but do not change the trust boundary.

So tier 2 — validating the real host environment — should use a known/vetted
drydock build, driven by the agent through the portal. New drydock code is
exercised in a VM.

## Tier 3: host-fidelity dev VM

When a change really must be validated against the host kernel, KVM, and store,
have the host boot a disposable **dev VM**:

- the VM config is pinned and vetted (not the agent's dirty working tree);
- nested virtualization is enabled for that VM;
- the drydock source is exposed read/write over virtiofs;
- the agent builds and runs drydock inside the VM.

The host only runs vetted drydock + QEMU; the agent's code stays in the VM. This
is the same pattern the tool uses for users: trusted code on the powerful
machine, agent code in a sandbox.

## What to add next

- A tier-3 `dev` NixOS configuration (Go, Nix, noVNC, nested KVM) and a portal
  operation to boot it from a pinned ref.
- Optional systemd hardening defaults for the host portal, as defense in depth
  for tier 2.
