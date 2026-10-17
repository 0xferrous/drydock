# Design

## The problem

An agent that can edit your OS configuration is useful, but you do not want it
to apply changes to the machine you are using. The fix is to always materialize
changes somewhere throwaway first, while keeping the loop fast enough that the
agent can iterate and you can inspect the result.

Two constraints shape every design:

1. **Fidelity.** The preview must be the *same* configuration, not a lookalike.
2. **Zero VM management.** The user's only interface is the agent; the agent's
   only interface should be a couple of commands.

## Form factors considered

### A. Persistent VM + live `switch-to-configuration` (chosen)

Boot one long-lived VM from the real config, edit the config, build the new
system on the host, and live-switch the guest. The guest shares the host Nix
store over 9p, so the new system closure is already visible inside the guest.

- **Pros:** fastest loop; GUI session (and its windows) persists across changes;
  only changed services restart; genuinely "type a change, see it happen".
- **Cons:** state can drift from a clean boot, so a change should also be
  verified with C before it is trusted.

### B. Disposable VM per change

`nix build ...system.build.vm`, boot a fresh VM for each revision, show it, then
destroy it.

- **Pros:** perfect reproducibility and isolation; impossible for one change to
  taint the next.
- **Cons:** a full boot per iteration (tens of seconds to minutes); no persistent
  session; worse for iterating on desktop changes.

### C. Snapshot / clean-room verification

Like A, but with a QCow2 snapshot (or a fresh disk) so a change can be rolled
back, and a "boot from scratch" mode to confirm it survives a cold boot.

- **Pros:** catches state-dependent bugs; gives a rollback safety net.
- **Cons:** more moving parts; snapshots of a running VM need care.

### D. Non-Nix / Ansible driver

Provision a cloud image with cloud-init, apply an Ansible playbook over SSH,
snapshot, present.

- **Pros:** works for Ansible-managed fleets that are not NixOS.
- **Cons:** "same OS config" is weaker unless the playbook fully describes the
  image; no store-sharing trick, so every change is a network apply.

## Chosen shape

`drydock` implements **A**, with **B/C as explicitly available exits**:

- `up` boots the persistent VM (A).
- `apply` builds and live-switches (A).
- `reset` destroys the disk and boots from scratch (C), and is the recommended
  final check for any change.
- `--fresh` is `reset` folded into `up`.

The non-Nix driver (D) is deliberately out of scope for now; the build step is
isolated in `Engine.buildAttr`, so a future `--driver ansible` could replace the
build/apply steps without touching the display or lifecycle layers.

## The store-sharing trick

The NixOS `vmVariant` supports `virtualisation.mountHostNixStore`, which mounts
the host `/nix/store` read-only into the guest over 9p and backs `/nix/store`
with a bind mount. That means:

- Building the new system on the host is enough; the guest can execute it
  immediately.
- `switch-to-configuration test` (or `switch`) inside the guest activates it
  without any copying.

`drydock apply` therefore builds the vm-variant toplevel and switches to it:

```sh
# drydock generates a wrapper flake that loads the user's flake as an input and
# extends the chosen nixosConfiguration with the guest module, then builds it:
nix build --impure --no-write-lock-file \
  -o "$VM_DIR/system" \
  "path:$VM_DIR/eval#drydock.system" \
  --override-input config "$FLAKE" \
  --override-input config/my-nix "$LOCAL_MY_NIX"

ssh root@127.0.0.1 -p "$SSH_PORT" \
  "$VM_DIR/system/bin/switch-to-configuration test"
```

Building the **vmVariant** toplevel (rather than the host toplevel) matters: it
carries the VM's filesystem and boot overrides, so switching inside the guest
does not fight the running system over mounts.

## Any config layout, any flake

The engine never assumes a single flake directory. It writes a tiny wrapper
flake that declares the user's config as the `config` input and exposes two
derivations with the guest module applied:

```nix
{
  inputs.config.url = "<user flake>";
  outputs = { self, config, ... }:
    let preview = (builtins.getAttr "<host>" config.nixosConfigurations)
                    .extendModules { modules = [ <guest> ]; };
    in {
      drydock.vm = preview.config.system.build.vm;
      drydock.system = preview.config.virtualisation.vmVariant.system.build.toplevel;
    };
}
```

Because this is a real `nix build` of a real flake, everything `nix` supports
works: `--override-input` (including transitive inputs such as
`config/my-nix`), and arbitrary extra flags via `--nix-arg`. A configuration
split across several directories and wired together with input overrides is
therefore indistinguishable from a single-flake setup. Overrides are stored in
the VM metadata so later `apply`/`reset` calls reproduce the same build.

## Display

`-display none -vnc 127.0.0.1:<port>` gives a headless VNC server. `noVNC`
(`novnc` in nixpkgs) serves a browser client and proxies websockets to VNC on a
second port. The proxy binds `--display-address` (default `127.0.0.1`) and the
CLI prints `http://<display-address>:<web>/vnc.html?autoconnect=1&resize=scale`.
Remote access is the caller's choice: bind a private/tailnet address, tunnel the
port, or reverse-proxy it.

`--headless` skips VNC entirely for pure service/CLI changes.

Screenshots use the QEMU monitor (`-qmp`) and `screendump`, so an agent can
"look" at the result without a browser.

## Guest module

`nix/guest/default.nix` is appended to the user's configuration via
`extendModules`. It adds only:

- `services.openssh` with root key auth and no passwords,
- `services.qemuGuest`,
- VM resources (`memorySize`, `cores`, `diskSize`, `resolution`), set through
  `virtualisation.vmVariant` because the base configuration never imports
  `qemu-vm.nix`.

Everything else is the user's own configuration.

## Guest agents and the host portal

When the coding agent runs inside a VM (an ash guest, in this setup), it cannot
reliably run a nested VM and does not hold the host Nix store. The preview VM
must therefore run on the physical host. Two things make this workable:

1. **The workspace is already shared.** Ash exposes the guest's workspaces to
   the host over virtiofs, so an edit the agent makes in the guest is immediately
   visible to the host at its own path. No upload step is needed.
2. **One API, two drivers.** The operation surface is the Go `Driver` interface
   in `internal/drydock/types.go`. `Engine` implements it once. The local driver
   calls the engine in-process; the remote driver reaches a portal that serves
   the same interface. The guest CLI parses flags into a `Request` and hands it
   to whichever driver is configured — there is no argv protocol and no second
   definition of the operations.

On the wire, streaming methods (`up`, `apply`, `reset`, `down`, `ssh`, `logs`,
`doctor`) return an NDJSON event stream; unary methods (`status`, `url`, `list`)
return JSON; `screenshot` returns a PNG. The portal always calls its local
`Driver`, so it can start and apply preview VMs but cannot run arbitrary host
commands. Binding to the private guest link (and an optional bearer token) keeps
other guests out.
