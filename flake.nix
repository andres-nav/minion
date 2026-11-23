{
  description = "Development environment for gru (the laptop CLI) and minion (the host runner)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    systems.url = "github:nix-systems/default";
  };

  outputs = { self, nixpkgs, systems }:
    let
      eachSystem = nixpkgs.lib.genAttrs (import systems);
    in
    {
      devShells = eachSystem (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              go
              gopls          # language server
              gotools        # goimports, etc.
              go-tools       # staticcheck
              cargo          # minion/
              rustc
              clippy
              gnumake
              git
            ];

            shellHook = ''
              echo "gru and minion development environment"
              echo "Go version: $(go version)"
              echo ""
              echo "Run 'make build' to compile gru and minion"
            '';
          };
        });
    };
}
