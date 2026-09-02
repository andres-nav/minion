{
  description = "Development environment for minion — a thin remote dev-loop orchestrator";

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
              gnumake
              git
            ];

            shellHook = ''
              echo "minion development environment"
              echo "Go version: $(go version)"
              echo ""
              echo "Run 'make build' to compile the minion binary"
            '';
          };
        });
    };
}
