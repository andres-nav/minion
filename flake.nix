{
  description = "Development environment for gru and minion";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }: {
    devShells = nixpkgs.lib.genAttrs [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ] (system:
      let pkgs = nixpkgs.legacyPackages.${system}; in {
        default = pkgs.mkShell { packages = with pkgs; [ go cargo rustc clippy git ]; };
      });
  };
}
