# Running the agent in a guest VM

If the coding agent itself runs inside a VM (for example an
[`ash`](https://github.com/0xferrous/ash) guest), do **not** run `drydock` inside
that guest. Nested KVM is unreliable, and the guest does not hold the real Nix
store, which is what makes `drydock apply` fast.

Instead, run `drydock` on the physical host and let the guest drive it over the
private host link.

```text
        physical host (KVM + real /nix/store)
        ┌────────────────────────────────────────────────┐
        │  drydock CLI ── boots preview VM (drydock)      │
        │       ▲                                         │
        │       │ narrow HTTP API                         │
        │  drydock-portal :8787                           │
        │       ▲                                         │
        │       │ 192.168.127.1 (ash0 bridge)             │
        └───────┼─────────────────────────────────────────┘
                │
   ash guest    │  /home/agent/dev/...  (virtiofs share)
        ┌───────┴─────────────────────────────────────────┐
        │  coding agent ── edits config (shared with host)│
        │  drydock --portal http://192.168.127.1:8787 ... │
        └─────────────────────────────────────────────────┘
```

Because ash exposes the workspace to the guest over virtiofs, the host already
sees every config edit the agent makes; nothing has to be uploaded.

## Host setup

Add the flake as an input and enable the module:

```nix
{
  inputs.drydock.url = "github:0xferrous/drydock";

  # ... in your host's nixosSystem modules:
  modules = [
    drydock.nixosModules.drydock-portal
    {
      services.drydock-portal = {
        enable = true;
        # The address the guest can reach (the ash0 bridge gateway).
        bindAddress = "192.168.127.1";
        port = 8787;
        # Optional: if the client may omit --flake/--host. These are host paths.
        defaultFlake = "/home/me/dev/mm/config";
        defaultHost = "dmnt";
        # Optional: require a bearer token (clients set DRYDOCK_PORTAL_TOKEN).
        tokenFile = "/run/secrets/drydock-portal-token";
        # Optional: keep all VM state outside the home directory.
        stateRoot = "/var/lib/drydock";
        # Optional: open the firewall port (or restrict it to the bridge interface).
        openFirewall = true;
        # Run as the user that owns the config and can access /dev/kvm.
        user = "me";
        # Conservative systemd sandboxing is on by default; disable or extend it
        # per host. `hardening` is defense in depth, not a trust boundary.
        # hardening = true;
        # readWritePaths = [ "/home/me/.cache/nix" ];
        # extraServiceConfig.ProtectSystem = "strict";
      };
    }
  ];
}
```

The service only ever executes the `drydock` executable; a client cannot run
arbitrary host commands through it. `hardening = true` (the default) applies
`NoNewPrivileges`, `PrivateTmp`, `ProtectSystem = "full"`, and kernel/SUID
protections. Going further (`ProtectSystem = "strict"`, `ProtectHome`) needs
`stateRoot` plus `readWritePaths` for anything else the portal writes (for
example the Nix client cache), so it is left to `extraServiceConfig`.

## Guest setup

Put `drydock` on the guest's `PATH` and point it at the portal:

```sh
export DRYDOCK_PORTAL=http://192.168.127.1:8787
# If the portal requires a token:
export DRYDOCK_PORTAL_TOKEN=...
```

Now the usual commands work unchanged and are executed on the host:

```sh
drydock up            # boots the preview VM on the host
drydock apply         # live-switches it
drydock screenshot /tmp/view.png
drydock down
```

`--portal` overrides the environment variable per invocation, and
`--name` selects a VM instance.

## Path mapping

When the portal has `defaultFlake`/`defaultHost`, clients can omit `--flake`
and `--host`. If a client does pass an absolute `--flake` path that only exists
inside the guest, add a mapping:

```nix
services.drydock-portal.pathMap = {
  "/home/agent/dev" = "/home/me/dev";
};
```

For a plain virtiofs share the two paths are usually the same directory on
disk, so this is frequently unnecessary. The map also applies to
`--override-input` refs, so a guest-visible override path is rewritten to its
host path.

## Sharing the display

The portal's noVNC server binds the `--display-address` given to `drydock`
(default `127.0.0.1` on the host). Bind the host/guest link address to let a
guest reach it, or a private/tailnet address to let a user reach it.

For anything else, tunnel or reverse-proxy the port yourself; the tool does not
prescribe a sharing mechanism.

## Security notes

- Bind the portal to the private host/guest link, never a public interface.
- Set `tokenFile` when more than one guest shares the bridge.
- The portal inherits the privileges of `services.drydock-portal.user`; it can
  build configs and run QEMU, so treat it like the `drydock` CLI itself.
