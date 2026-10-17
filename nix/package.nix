{
  lib,
  buildGoModule,
  makeWrapper,
  coreutils,
  novnc,
  openssh,
  nix,
}:
buildGoModule {
  pname = "drydock";
  version = "0.2.0";
  src = ./..;
  vendorHash = null;
  subPackages = [ "cmd/drydock" ];

  nativeBuildInputs = [ makeWrapper ];
  postInstall = ''
    wrapProgram $out/bin/drydock \
      --prefix PATH : ${
        lib.makeBinPath [
          coreutils
          novnc
          openssh
          nix
        ]
      } \
      --set DRYDOCK_NIX_DIR ${./.}
  '';

  meta = {
    description = "Preview NixOS configuration changes in a VM";
    mainProgram = "drydock";
  };
}
