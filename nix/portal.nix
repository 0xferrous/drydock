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
  pname = "drydock-portal";
  version = "0.2.0";
  src = ./..;
  vendorHash = null;
  subPackages = [ "cmd/drydock-portal" ];

  nativeBuildInputs = [ makeWrapper ];
  postInstall = ''
    wrapProgram $out/bin/drydock-portal \
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
    description = "Host-side HTTP portal that serves the drydock driver to a guest agent";
    mainProgram = "drydock-portal";
  };
}
