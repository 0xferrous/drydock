{
  description = "drydock — preview NixOS configuration changes in a VM, driven by your agent";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: rec {
        drydock = pkgs.callPackage ./nix/package.nix { };
        drydock-portal = pkgs.callPackage ./nix/portal.nix { };
        default = drydock;
      });

      checks = forAllSystems (pkgs: {
        unit =
          pkgs.runCommand "drydock-unit-tests"
            {
              nativeBuildInputs = [ pkgs.go ];
              src = self;
            }
            ''
              export HOME=$TMPDIR
              export GOCACHE=$TMPDIR/go-cache
              export CGO_ENABLED=0
              cp -r $src source
              chmod -R u+w source
              cd source
              go test ./...
              touch $out
            '';
      });

      apps = forAllSystems (pkgs: rec {
        drydock = {
          type = "app";
          program = "${self.packages.${pkgs.stdenv.hostPlatform.system}.drydock}/bin/drydock";
          meta.description = "Preview NixOS configuration changes in a VM";
        };
        default = drydock;
      });

      # Host-side portal: see nix/module.nix.
      nixosModules.default = import ./nix/module.nix;
      nixosModules.drydock-portal = import ./nix/module.nix;

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go
            just
            nixfmt-tree
            shellcheck
          ];
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt-tree);
    };
}
