# AGENTS

`drydock` previews NixOS configuration changes in a VM. The user's only
interface is an agent; the agent's interface is the `drydock` CLI plus the skill
in `skills/drydock/SKILL.md`.

Everything is Go, and the operation surface is declared once as the `Driver`
interface in `internal/drydock/types.go`.

## Layout

- `internal/drydock/types.go` — the API: `Request`, `Status`, `Event`, `Sink`,
  and the `Driver` interface. This is the single definition of the operations.
- `internal/drydock/engine.go` — `Engine`, the one implementation of `Driver`
  (lifecycle, eval, SSH, logs, list, doctor).
- `internal/drydock/display.go` — noVNC startup and the QMP screenshot/PPM
  capture.
- `internal/drydock/rpc.go` — the portal `Server` and the `Remote` driver; both
  speak the same JSON/NDJSON protocol derived from `Driver`.
- `internal/drydock/cli.go` — flag parsing and command dispatch against a
  `Driver` (local `Engine` or `Remote`).
- `cmd/drydock` — the CLI binary; `cmd/drydock-portal` — the host portal binary.
- `nix/package.nix` / `nix/portal.nix` — build the two Go binaries and wrap them
  with their runtime tools and `DRYDOCK_NIX_DIR`.
- `nix/module.nix` — `nixosModules.drydock-portal`, the host systemd service.
- `nix/guest/default.nix` — the only module injected into the user's config
  (SSH, qemu-guest-agent, VM sizing). The engine writes a per-VM wrapper flake
  (`$VM_DIR/eval/flake.nix`) that loads the user's flake as `config` and extends
  the chosen `nixosConfiguration` with this module, so that a real
  `nix build` (with `--override-input`) does the work.
- `demo/` — a self-contained minimal NixOS desktop for testing the tool.
- `docs/design.md` — form factors and rationale. Read it before changing the
  architecture.
- `docs/portal.md` — host/guest split setup.
- `skills/drydock/SKILL.md` — the agent-facing instructions.

## Rules

- Never make `drydock` run `nixos-rebuild switch` (or anything that activates a
  generation) on the host. It may only run pure `nix build` and QEMU.
- Keep the injected guest module minimal; it runs inside the user's real config.
- VM-only options (`virtualisation.memorySize`, `cores`, `diskSize`,
  `resolution`) must be set under `virtualisation.vmVariant`, because the base
  configuration does not import `qemu-vm.nix`.
- Build `config.virtualisation.vmVariant.system.build.toplevel` for `apply`, so
  the switched system matches the running guest's filesystems.
- Prefer clear failures with a pointer to the console log over silent fallbacks.
- Add operations to `Driver` once; the engine, the portal, and the remote driver
  all follow from it. Do not introduce a second, argv-shaped protocol.
- Never assume a single flake directory or a fixed input set. Build through the
  generated wrapper flake so `--override-input`, `--nix-arg`, and arbitrary
  config layouts work. Overrides must be persisted in the VM metadata.
- The portal only ever calls the local `Driver`. Never add a generic shell or
  arbitrary-command endpoint.
- Keep the Go code `gofmt`-clean.

## Testing

See `docs/testing.md`. The rule is: **the host runs only vetted code;
agent-authored code runs in a VM.**

Before handing a build to the user, run the end-to-end check (needs KVM, takes
a few minutes). It covers fresh boot, re-`up` while healthy, screenshot, live
`apply`, recovery from a broken guest, `down`, and `--dry-run`:

```sh
just e2e
```

Tier 0 (no KVM):

```sh
nix flake check                 # builds and runs the unit tests
```

Tier 1 (inside the disposable agent VM):

```sh
nix build .#drydock -o result
./result/bin/drydock up --flake ./demo --host demo --dry-run  # build only
./result/bin/drydock up --flake ./demo --host demo
./result/bin/drydock apply
./result/bin/drydock screenshot /tmp/drydock.png
./result/bin/drydock reset --flake ./demo --host demo
./result/bin/drydock down
```

Portal (both ends on one machine is enough to exercise the protocol):

```sh
nix build .#drydock-portal -o result-portal
./result-portal/bin/drydock-portal --bind 127.0.0.1 --port 8787 \
  --default-flake "$PWD/demo" --default-host demo &
DRYDOCK_PORTAL=http://127.0.0.1:8787 ./result/bin/drydock up
DRYDOCK_PORTAL=http://127.0.0.1:8787 ./result/bin/drydock screenshot /tmp/shot.png
DRYDOCK_PORTAL=http://127.0.0.1:8787 ./result/bin/drydock down
```

Run `nix fmt` before committing.
