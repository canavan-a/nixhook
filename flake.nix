{
  description = "nixhook — deploy NixOS flake configurations over HTTP";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      version = self.shortRev or self.dirtyShortRev or "dev";
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: {
        default = self.packages.${pkgs.stdenv.hostPlatform.system}.nixhook;
        nixhook = pkgs.buildGoModule {
          pname = "nixhook";
          inherit version;
          src = ./.;
          subPackages = [ "cmd/nixhook" ];
          vendorHash = null;
          env.CGO_ENABLED = "0";
          ldflags = [ "-s" "-w" "-X main.Version=${version}" ];
          meta = {
            description = "Deploy NixOS flake configurations over HTTP";
            license = nixpkgs.lib.licenses.mit;
            mainProgram = "nixhook";
          };
        };
      });

      nixosModules.default = { pkgs, lib, ... }: {
        imports = [ ./nix/module.nix ];
        services.nixhook.package = lib.mkDefault self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      };

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell { buildInputs = with pkgs; [ go gopls gotools ]; };
      });
    };
}
