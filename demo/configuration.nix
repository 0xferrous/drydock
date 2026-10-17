# A deliberately tiny X11 desktop. It exists so `drydock up --flake ./demo
# --host demo` produces something you can actually look at and click on over
# VNC. Replace it with your real nixosConfiguration.
{
  pkgs,
  markerValue,
  ...
}:
{
  system.stateVersion = "24.05";
  networking.hostName = "drydock-demo";

  # Written from a flake input so `--override-input marker <ref>` is visible:
  #   drydock ssh -- cat /etc/drydock-marker
  environment.etc."drydock-marker".text = markerValue;

  services.xserver = {
    enable = true;
    displayManager.lightdm.enable = true;
    windowManager.fluxbox.enable = true;
    desktopManager.xterm.enable = true;
  };
  services.displayManager.autoLogin = {
    enable = true;
    user = "demo";
  };

  users.users.demo = {
    isNormalUser = true;
    password = "demo";
    extraGroups = [ "wheel" ];
  };
  security.sudo.wheelNeedsPassword = false;

  # Something for `drydock apply` to change on the first try:
  # edit this list and run `drydock apply`.
  environment.systemPackages = with pkgs; [
    cowsay
    figlet
    toilet
    lolcat
    hello
    sl
  ];
}
