# drydock guest module.
#
# This is appended to the user's own nixosConfiguration, so the preview is
# built from the real config plus the minimum needed to manage it: SSH for
# live-switching and a QEMU guest agent. It does not change the desktop,
# services, or packages being previewed.
#
# Values are supplied by the generated wrapper flake as module options rather
# than `builtins.getEnv`, so the host build can run in pure evaluation mode.
{ config, lib, ... }:
let
  cfg = config.drydock;
in
{
  options.drydock = {
    sshPubkey = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Public key authorized for root SSH in the preview VM.";
    };

    mem = lib.mkOption {
      type = lib.types.nullOr lib.types.int;
      default = null;
      description = "Guest memory in MiB.";
    };

    cpus = lib.mkOption {
      type = lib.types.nullOr lib.types.int;
      default = null;
      description = "Guest vCPUs.";
    };

    disk = lib.mkOption {
      type = lib.types.nullOr lib.types.int;
      default = null;
      description = "Guest disk size in MiB.";
    };
  };

  config = {
    services.openssh = {
      enable = lib.mkForce true;
      openFirewall = lib.mkForce true;
      settings = {
        PermitRootLogin = lib.mkForce "prohibit-password";
        PasswordAuthentication = lib.mkForce false;
        KbdInteractiveAuthentication = lib.mkForce false;
      };
    };

    users.users.root.openssh.authorizedKeys.keys = lib.mkIf (cfg.sshPubkey != null) [
      (lib.removeSuffix "\n" cfg.sshPubkey)
    ];

    services.qemuGuest.enable = lib.mkForce true;

    # drydock owns the VM variant. A user's own `virtualisation.vmVariant`
    # settings (a GTK/GL display, extra host directory shares, and so on) are
    # for `nix build ...system.build.vm` on their desktop and are wrong for a
    # headless VNC preview. Forcing this also strips any extra host directory
    # shares an untrusted config might add.
    virtualisation.vmVariant = lib.mkForce {
      virtualisation = {
        graphics = true;
        resolution = {
          x = 1600;
          y = 900;
        };
        memorySize = if cfg.mem == null then 4096 else cfg.mem;
        cores = if cfg.cpus == null then 4 else cfg.cpus;
        diskSize = if cfg.disk == null then 16384 else cfg.disk;
      };
    };
  };
}
