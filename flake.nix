{
  description = "dev-inbox: terminal mimic of the GitHub PR inbox with an event stream";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { nixpkgs, ... }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: {
        default = pkgs.buildGo126Module rec {
          pname = "dev-inbox";
          # The release workflow refuses a tag whose version differs from this one.
          version = "0.2.1";
          src = ./.;
          subPackages = [
            "cmd/dev-inbox"
            "cmd/dev-inbox-jev"
            "cmd/dev-inbox-linear"
            "cmd/dev-inbox-slack"
          ];
          vendorHash = "sha256-wRgpU3YxqPnU319C1Zown338QtQ2ocnjx83GuO+ONKs=";
          env.CGO_ENABLED = "0";
          ldflags = [
            "-s"
            "-w"
            "-X main.version=${version}"
          ];
          # The default checkPhase only tests subPackages, which would skip every internal package.
          checkPhase = ''
            runHook preCheck
            export GOFLAGS=''${GOFLAGS//-trimpath/}
            buildGoDir test ./...
            runHook postCheck
          '';
          meta = {
            description = "Terminal mimic of the GitHub PR inbox backed by an event-driven daemon";
            license = pkgs.lib.licenses.mit;
            mainProgram = "dev-inbox";
          };
        };
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go_1_26
            gopls
            golangci-lint
            just
            natscli
          ];
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
