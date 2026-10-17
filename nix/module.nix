# NixOS module: run the drydock HTTP portal on the physical host.
#
# The agent runs inside a guest VM that cannot usefully run nested VMs, so the
# preview VM must be built and booted here, on the machine with KVM and the
# real Nix store. This service exposes a narrow HTTP API on the host's private
# guest link; the same `drydock` CLI on the guest talks to it with
# `DRYDOCK_PORTAL` set.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.drydock-portal;
  defaultPackage = pkgs.callPackage ./portal.nix { };

  # Where the portal keeps VM state. systemd creates and owns this when it is
  # the default, keeping it writable even under ProtectSystem = "strict".
  stateDir = if cfg.stateRoot != null then cfg.stateRoot else "/var/lib/drydock";

  # The portal runs `nix build` and QEMU on the host, so it is sandboxed by
  # default. It can read the config (in the home) but not write it, and can
  # only write its own state directory. QEMU runs as this user, so the sandbox
  # also bounds what a malicious config can reach through the guest.
  hardeningDefault = {
    NoNewPrivileges = true;
    PrivateTmp = true;
    ProtectSystem = "strict";
    ProtectHome = "read-only";
    ProtectKernelTunables = true;
    ProtectKernelModules = true;
    ProtectKernelLogs = true;
    ProtectClock = true;
    ProtectHostname = true;
    RestrictAddressFamilies = [
      "AF_UNIX"
      "AF_INET"
      "AF_INET6"
      "AF_NETLINK"
    ];
    RestrictSUIDSGID = true;
    RestrictRealtime = true;
    SystemCallArchitectures = "native";
  };
in
{
  options.services.drydock-portal = {
    enable = lib.mkEnableOption "the drydock host portal";

    package = lib.mkOption {
      type = lib.types.package;
      default = defaultPackage;
      defaultText = lib.literalExpression "drydock-portal";
      description = "The drydock-portal package to run.";
    };

    bindAddress = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1";
      example = "192.168.127.1";
      description = ''
        Address to listen on. For an Ash-style guest network, use the host
        bridge address (for example `192.168.127.1`) so guests can reach it.
      '';
    };

    port = lib.mkOption {
      type = lib.types.port;
      default = 8787;
      description = "Port to listen on.";
    };

    defaultFlake = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "/home/me/dev/mm/config";
      description = ''
        Flake to build when a client does not pass `--flake`. The path is the
        *host* path, which is what the portal can see.
      '';
    };

    defaultHost = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "dmnt";
      description = "nixosConfiguration to preview when a client does not pass `--host`.";
    };

    defaultOverrideInputs = lib.mkOption {
      type = lib.types.attrsOf lib.types.str;
      default = { };
      example = {
        my-nix = "/home/me/dev/fr/my-nix";
      };
      description = ''
        Input overrides applied when a client does not pass `--override-input`.
        Use host paths. A per-VM override saved by `drydock up` takes
        precedence over these defaults.
      '';
    };

    pathMap = lib.mkOption {
      type = lib.types.attrsOf lib.types.str;
      default = { };
      example = {
        "/home/agent/dev" = "/home/me/dev";
      };
      description = ''
        Rewrite guest-visible path prefixes to host paths. Only needed when a
        client passes an absolute `--flake` path that differs between the two
        machines and the portal has no `defaultFlake`.
      '';
    };

    cwd = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Working directory for drydock invocations (defaults to `defaultFlake`).";
    };

    tokenFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = ''
        File containing a bearer token. When set, requests must send
        `Authorization: Bearer <token>`, and clients set `DRYDOCK_PORTAL_TOKEN`.
      '';
    };

    user = lib.mkOption {
      type = lib.types.str;
      default = "root";
      description = ''
        User to run the portal as. Use the host user that owns the config and
        has KVM access; root works for a purely local preview.
      '';
    };

    openFirewall = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Open `port` in the host firewall.";
    };

    extraArgs = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Extra arguments passed to drydock-portal.";
    };

    stateRoot = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "/var/lib/drydock";
      description = ''
        Directory for drydock's per-VM state (disk images, metadata, keys).
        Sets `DRYDOCK_STATE`. Defaults to `/var/lib/drydock`, which systemd
        creates and keeps writable under the sandbox. Point it elsewhere only
        if that directory already exists and is writable by the service user.
      '';
    };

    hardening = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Sandbox the portal with systemd. With this on the portal can read the
        config but not write the home, and can only write its state directory.
        QEMU runs as the same user, so this also bounds what a malicious config
        can reach through the guest. Set to `false` only if a directive
        interferes with your setup.
      '';
    };

    allowNixArgs = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = ''
        Allow clients to pass `--nix-arg` through to `nix build`. This is
        unsafe against an untrusted agent: it can disable the Nix sandbox or
        change build settings. Leave disabled unless the agent is trusted.
      '';
    };

    readWritePaths = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = ''
        Extra paths the portal may write to when `hardening = true`, in
        addition to `stateRoot`.
      '';
    };

    extraServiceConfig = lib.mkOption {
      type = lib.types.attrs;
      default = { };
      description = "Extra systemd `serviceConfig` entries for the portal.";
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.drydock-portal = {
      description = "drydock host portal";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      serviceConfig = {
        ExecStart = lib.concatStringsSep " " (
          [
            "${cfg.package}/bin/drydock-portal"
            "--bind"
            cfg.bindAddress
            "--port"
            (toString cfg.port)
          ]
          ++ lib.optionals (cfg.defaultFlake != null) [
            "--default-flake"
            cfg.defaultFlake
          ]
          ++ lib.optionals (cfg.defaultHost != null) [
            "--default-host"
            cfg.defaultHost
          ]
          ++ lib.optionals (cfg.cwd != null) [
            "--cwd"
            cfg.cwd
          ]
          ++ lib.optionals (cfg.tokenFile != null) [
            "--token-file"
            (toString cfg.tokenFile)
          ]
          ++ lib.concatLists (
            lib.mapAttrsToList (from: to: [
              "--map"
              "${from}=${to}"
            ]) cfg.pathMap
          )
          ++ lib.concatLists (
            lib.mapAttrsToList (name: ref: [
              "--default-override-input"
              "${name}=${ref}"
            ]) cfg.defaultOverrideInputs
          )
          ++ lib.optionals cfg.allowNixArgs [ "--allow-nix-arg" ]
          ++ cfg.extraArgs
        );
        User = cfg.user;
        Restart = "on-failure";
        RestartSec = 3;
        # QEMU needs /dev/kvm when the portal does not run as root.
        SupplementaryGroups = [ "kvm" ];
        # Keep VM state and the Nix eval cache inside the writable state dir.
        Environment = [
          "DRYDOCK_STATE=${stateDir}"
          "XDG_CACHE_HOME=${stateDir}/cache"
        ];
      }
      // lib.optionalAttrs cfg.hardening (
        hardeningDefault
        // lib.optionalAttrs (cfg.stateRoot == null) {
          StateDirectory = "drydock";
        }
        // {
          ReadWritePaths = [ stateDir ] ++ cfg.readWritePaths;
        }
      )
      // cfg.extraServiceConfig;
    };

    networking.firewall.allowedTCPPorts = lib.mkIf cfg.openFirewall [ cfg.port ];
  };
}
