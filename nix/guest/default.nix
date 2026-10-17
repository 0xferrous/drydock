# drydock guest module.
#
# This is appended to the user's own nixosConfiguration, so the preview is
# built from the real config plus the minimum needed to manage it: SSH for
# live-switching and a QEMU guest agent. It deliberately does not change the
# desktop, services, or packages that the user is previewing.
{ lib, ... }:
let
  # Values are supplied impurely by the drydock CLI before evaluation.
  getEnv =
    name:
    let
      value = builtins.getEnv name;
    in
    if value == "" then null else value;

  pubkeyFile = getEnv "DRYDOCK_SSH_PUBKEY_FILE";
  pubkey = if pubkeyFile == null then null else lib.removeSuffix "\n" (builtins.readFile pubkeyFile);

  mem = getEnv "DRYDOCK_MEM";
  cpus = getEnv "DRYDOCK_CPUS";
  disk = getEnv "DRYDOCK_DISK";

  toIntOrDefault = value: default: if value == null then default else lib.toInt value;
in
{
  # Root key access lets drydock apply a rebuilt configuration without a
  # password and without a console. The VM is a sandbox; nothing else is exposed.
  services.openssh = {
    enable = lib.mkForce true;
    openFirewall = lib.mkForce true;
    settings = {
      PermitRootLogin = lib.mkForce "prohibit-password";
      PasswordAuthentication = lib.mkForce false;
      KbdInteractiveAuthentication = lib.mkForce false;
    };
  };

  users.users.root.openssh.authorizedKeys.keys = lib.mkIf (pubkey != null) [ pubkey ];

  services.qemuGuest.enable = lib.mkForce true;

  # Sensible, overridable VM resources and a consistent framebuffer. These
  # options only exist inside the `vmVariant`, so they must be set there rather
  # than on the base configuration (which never imports qemu-vm.nix).
  virtualisation.vmVariant.virtualisation = {
    resolution = lib.mkDefault {
      x = 1600;
      y = 900;
    };
    memorySize = lib.mkDefault (toIntOrDefault mem 4096);
    cores = lib.mkDefault (toIntOrDefault cpus 4);
    diskSize = lib.mkDefault (toIntOrDefault disk 16384);
  };
}
