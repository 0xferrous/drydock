---
name: drydock
description: "Preview operating-system configuration changes in a throwaway NixOS VM instead of applying them to the host. Use when the user asks to change system configuration, test a NixOS/Home Manager change, add a service or package that affects the OS, or preview a desktop/GUI change safely. The user describes the change; you edit the config, apply it to the VM, and present the result."
---

# drydock — preview OS changes in a VM

`drydock` builds and boots the user's own `nixosConfiguration` as a VM, applies
their config edits to that VM, and gives back a browser-accessible display. The
host system is never modified.

## Hard rules

1. **Never apply configuration changes to the host.** Do not run
   `nixos-rebuild switch`, `nixos-rebuild boot`, `home-manager switch`, or any
   command that activates a new generation on the host.
2. **Only edit the configuration flake.** All changes go through the flake and
   `drydock apply`.
3. **Do not manage the VM by hand.** Use `drydock up` / `apply` / `reset` /
   `down`. Do not `qemu`, `kill`, or `ssh` into QEMU outside `drydock` unless
   `drydock` itself is broken.
4. **Do not invent a display URL.** Use `drydock url` or `drydock expose`
   output.

## Prerequisites

Confirm once per host:

```sh
drydock doctor
```

**You may be running inside a guest VM.** If so, `drydock` must execute on the
physical host, not here. When `DRYDOCK_PORTAL` is set in your environment the
same commands transparently proxy to a host portal, and you do not need to know
the flake or host path — the portal supplies defaults. If it is not set but you
know the portal URL, pass `--portal http://<host>:8787` on each command.

If `drydock` is not on PATH, run it from its project:

```sh
nix run <path-to-drydock> -- <command> ...
```

Otherwise you need the user's **config flake path** and **nixosConfiguration
name** (for example `~/dev/mm/config` and `dmnt`). Ask if unknown; do not guess.
When a portal is configured with defaults, omit `--flake` and `--host`.

If the config pulls in other local checkouts through flake inputs (for example a
`my-nix` input pointing at a local directory), pass the same overrides the user
uses for their real installation:

```sh
drydock up --flake ~/dev/mm/config --host dmnt \
  --override-input my-nix ~/dev/fr/my-nix
```

Overrides are remembered per VM, so later `apply`/`reset` calls reproduce them
without repeating the flags.

## Workflow

1. **Bring up the preview.**

   ```sh
   drydock up --flake <config-flake> --host <host>
   ```

   Add `--headless` only when the change cannot affect anything graphical.

2. **Make the change** by editing the flake the way the user would. Keep a
   normal, reviewable diff.

3. **Apply it live.** No reboot is needed for userspace changes:

   ```sh
   drydock apply --flake <config-flake> --host <host>
   ```

4. **Verify.**
   - GUI change: capture the screen so you can inspect it yourself.

     ```sh
     drydock screenshot /tmp/drydock.png
     ```

     Read the PNG back and check that the change actually appears. Then present
     the interactive display (next step).
   - Non-GUI change: verify inside the guest, e.g.
     `drydock ssh -- 'systemctl status foo'` or run the changed command.

5. **Present it to the user.**
   - Interactive display:

     ```sh
     drydock url
     ```

     Give the URL as a Markdown link. If it is bound to localhost and you need
     to reach it from elsewhere, restart with `--display-address <address>` or
     forward the port yourself (for example `ssh -L` or `socat`) and share the
     forwarded address.
   - Non-GUI change: give a short summary plus the relevant verified output or
     screenshot.

6. **Offer a clean-room check** for anything non-trivial:

   ```sh
   drydock reset --flake <config-flake> --host <host>
   ```

   This reprovisions the disk and boots from scratch, proving the change does
   not depend on accumulated state.

7. **Tear down when done** (unless the user wants to keep exploring):

   ```sh
   drydock down
   ```

## Useful commands

| Command | Use |
| --- | --- |
| `drydock status` | Is the VM running, and on which URL? |
| `drydock url` | The interactive noVNC URL. |
| `drydock screenshot FILE` | Capture the guest display. |
| `drydock ssh -- CMD` | Inspect the guest. |
| `drydock logs` | Guest console output when boot or switch fails. |
| `drydock apply --persist` | Make the switch survive a reboot. |

## Notes

- Kernel, initrd, or bootloader changes need `drydock reset`; `apply` only
  updates userspace.
- If `drydock apply` fails, read `drydock logs`, fix the config, and retry. Do
  not fall back to activating on the host.
- The VM stores state under `~/.local/state/drydock/vms/<name>/`. Multiple
  named VMs can run at once with `--name`.
