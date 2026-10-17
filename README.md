# drydock

Preview operating-system configuration changes in a real VM, driven entirely by
your coding agent.

You describe a change. The agent edits your config, `drydock` boots that exact
configuration as a VM, live-switches the running guest to the edited config, and
hands you a browser window into the result. Your host system is never touched.

```text
  you ──"add foo to the bar host"──▶ agent
                                      │ edits your flake
                                      ▼
                                   drydock up/apply
                                      │ nix builds system.build.vm
                                      ▼
                          ┌───────────────────────────┐
                          │  NixOS guest (your cfg)   │
                          │  + SSH + qemu-guest-agent │
                          │  VNC → noVNC → browser    │
                          └───────────────────────────┘
                                      │
                       live switch-to-configuration (no reboot)
```

## Why it is fast and safe

- **It is your config.** `drydock` evaluates your existing
  `nixosConfigurations.<host>` and extends it with a tiny guest module
  (`nix/guest/default.nix`). Nothing about your desktop, services, or packages
  is rewritten.
- **The Nix store is shared, not copied.** The NixOS VM mounts the host store
  read-only over 9p. When the agent edits the config, `drydock apply` builds the
  new system *on the host store* and the guest switches straight to it. No
  closure upload, no full reboot, no waiting for `nixos-rebuild` inside the VM.
- **The host is never modified.** `drydock` only runs `nix build` (a pure
  build) and QEMU. It never runs `nixos-rebuild switch` against your machine.
- **The disk is disposable.** Everything lives under the VM's state directory;
  `drydock reset` throws it away and boots a clean room.

## Requirements

- A Nix flake with at least one `nixosConfigurations.<host>`.
- `/dev/kvm`.
- A NixOS host (or any Linux host with Nix and KVM) with a running user systemd.

## Quickstart

```sh
# 1. Boot the bundled demo desktop
nix run . -- up --flake ./demo --host demo

# 2. Open the printed display URL in a browser, or ask the agent to
nix run . -- url

# 3. Change something and apply it live
$EDITOR demo/configuration.nix
nix run . -- apply

# 4. Tear it down
nix run . -- down
```

Point it at your real configuration instead:

```sh
nix run . -- up --flake ~/dev/mm/config --host dmnt
```

## Commands

| Command | Purpose |
| --- | --- |
| `up` | Build and boot the preview VM (idempotent). Starts VNC + noVNC unless `--headless`. |
| `apply` | Rebuild the config and live-switch the running guest. |
| `reset` | Destroy the guest disk and boot fresh. Use before trusting a change. |
| `down` | Stop the VM and its display. |
| `rm` | Stop (if needed) and delete a VM's state. |
| `prune` | Delete every stopped VM's state. |
| `status` | One VM's state, config, overrides, resources, ports, disk/RSS and URLs (`--json`). |
| `url` | Print the browser (noVNC) URL. |
| `screenshot [FILE]` | Capture the guest framebuffer to a PNG (QMP `screendump`). |
| `ssh [-- CMD]` | Shell into the guest, or run one command. |
| `logs` | Follow the guest console. |
| `list` | List VMs with config and display (`--json`, `--running`). |
| `doctor` | Check host prerequisites. |

Options: `--name`, `--flake`, `--host`, `--mem`, `--cpus`, `--disk`,
`--display-address`, `--override-input`, `--nix-arg`, `--headless`, `--fresh`,
`--persist`, `--json`, `--portal`.

## Any config layout

drydock does not assume a single-directory flake. It loads your flake as an
input and extends the named `nixosConfiguration` with the guest module, then
builds it as a normal `nix build`. That means everything `nix` supports works,
including input overrides:

```sh
# A config split across two directories, with my-nix overridden to the local
# checkout (exactly how you would run nixos-rebuild on it):
drydock up \
  --flake ~/dev/mm/config --host dmnt \
  --override-input my-nix ~/dev/fr/my-nix
```

Pass any other `nix build` flag with `--nix-arg` (repeatable). Overrides are
remembered per VM, so later `drydock apply` and `drydock reset` reproduce the
same build without repeating them.

## Running the agent inside a VM (portal)

If the coding agent runs inside a guest VM, run `drydock` on the physical host
instead of nesting VMs. The guest drives it through a narrow HTTP API:

```text
host:  drydock  +  drydock-portal :8787
                    ▲
                    │ private host link
                    ▼
guest: agent  ──  drydock --portal http://<host>:8787 ...
```

The same CLI is used on both sides: with `DRYDOCK_PORTAL` set it proxies every
command to the host, so `drydock up`, `apply`, `screenshot`, and `down` behave
exactly as before. A NixOS module (`nixosModules.drydock-portal`) runs the host
service. Because ash shares the workspace over virtiofs, the host already sees
the config the agent edits.

See [`docs/portal.md`](./docs/portal.md) for the full setup.

## For agents

The intended interface is a skill, not a CLI the user memorizes. The skill in
[`skills/drydock/SKILL.md`](./skills/drydock/SKILL.md) tells an agent to:

1. Never touch the host configuration generation — only edit the flake.
2. Bring up the preview VM and expose its display.
3. Apply edits with `drydock apply` and verify with `drydock screenshot`.
4. Hand the user a clickable display link, or a screenshot for non-GUI changes.

Install it by copying or symlinking the skill into your agent's skill directory.

## Safety model

- The VM has a generated root SSH key known only to `drydock`; no passwords.
- SSH and the raw VNC socket bind `127.0.0.1`. The noVNC web display binds
  `--display-address` (default `127.0.0.1`); point it at a private, tunneled, or
  tailnet address to reach it remotely, and keep it off public interfaces.
- The guest's Nix store is a read-only view of the host store, so the guest
  cannot corrupt your system's store.
- `--persist` is required to make a switch survive a reboot; the default
  `test` action is intentionally ephemeral.

## Running with an untrusted agent

The host portal is built to be safe when the agent on the other end is not
trusted:

- **Pure evaluation.** The host build runs without `--impure`, and guest
  parameters are passed as module options rather than `builtins.getEnv`. A
  client-controlled config therefore cannot call `getEnv` or `readFile` on
  arbitrary host paths.
- **Forced Nix sandbox.** Every build passes `--option sandbox true`, ignoring
  any client attempt to weaken it.
- **No `--nix-arg` from clients.** The portal rejects it by default; only a
  local, trusted `drydock` (or an explicitly enabled portal) may pass extra
  `nix build` flags.
- **Validated input.** VM names, `--override-input` names, and display
  addresses are validated before they reach paths, unit names, or argv.
- **Sandboxed service.** Run through `nixosModules.drydock-portal`, the portal
  has `ProtectSystem=strict`, `ProtectHome=read-only`, `NoNewPrivileges`, and a
  single writable `StateDirectory` (`/var/lib/drydock`). QEMU runs as that same
  user, so host directories exposed through guest shares cannot be written.

Residual risk: the config is still built (inside the Nix sandbox) and executed
inside the guest, and the Nix daemon and kernel are trusted. A config that is
built is still an attack surface; the sandbox is what bounds it.

## Limitations

- Kernel, initrd, and bootloader changes are not live-switched. `apply` updates
  userspace; run `reset` to rebuild and reboot for boot-level changes.
- Hardware-specific behavior (Wi-Fi, fingerprint, lid, GPU quirks) is not
  faithfully reproduced. `drydock` is for config and desktop validation.
- GUI output depends on the guest compositor. X11 and Wayland both work; a
  compositor that renders to the DRM device will appear over VNC.

## Design notes

See [`docs/design.md`](./docs/design.md) for the form factors considered and why
this one was chosen, and [`docs/testing.md`](./docs/testing.md) for how drydock
itself is tested safely.
