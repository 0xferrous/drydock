{
  description = "A minimal NixOS desktop used to try drydock";

  inputs = {
    # Pinned to the revision this host was built from so the demo reuses the
    # local store and the binary cache. Replace with your own config anyway.
    nixpkgs.url = "github:NixOS/nixpkgs/a430a19c1b8e4ca4e8ac512d94a7453f04955d45";
    # A tiny input used to demonstrate `--override-input`.
    marker.url = "path:./marker";
  };

  outputs =
    {
      self,
      nixpkgs,
      marker,
    }:
    {
      nixosConfigurations.demo = nixpkgs.lib.nixosSystem {
        system = "x86_64-linux";
        specialArgs = {
          markerValue = marker.value;
        };
        modules = [ ./configuration.nix ];
      };
    };
}
