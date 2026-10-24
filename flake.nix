{
  description = "domainchecker: a Squid external_acl_type helper that puts a human in the loop for proxy requests";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
  };

  outputs =
    {
      self,
      nixpkgs,
    }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;

      mkPackages =
        pkgs:
        let
          version = "0.1.0";
          src = self;
          vendorHash = "sha256-0Qxw+MUYVgzgWB8vi3HBYtVXSq/btfh4ZfV/m1chNrA=";

          # mkDomainchecker builds subPackage (one of this module's two
          # cmd/main.go directories) and renames its output binary from
          # buildGoModule's default -- "main", the basename of both
          # server/main and client/main, since both dirs happen to share
          # that name -- to a distinct, meaningful one. Without the
          # rename, building both subPackages into one derivation would
          # have them clobber each other at $out/bin/main.
          mkDomainchecker =
            {
              pname,
              subPackage,
            }:
            pkgs.buildGoModule {
              inherit pname version src vendorHash;
              subPackages = [ subPackage ];
              env.CGO_ENABLED = 0;
              # The test suite covers real wall-clock timeouts and
              # testing/synctest-driven concurrency; CI (.github/workflows/go.yml)
              # is the actual gate for that. Running it again in every Nix
              # build would just slow the build down for no added safety.
              doCheck = false;
              postInstall = ''
                mv $out/bin/main $out/bin/${pname}
              '';
            };
        in
        {
          domainchecker-server = mkDomainchecker {
            pname = "domainchecker-server";
            subPackage = "server/main";
          };
          domainchecker-client = mkDomainchecker {
            pname = "domainchecker-client";
            subPackage = "client/main";
          };
        };
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          built = mkPackages pkgs;
        in
        built
        // {
          default = built.domainchecker-server;
        }
      );

      apps = forAllSystems (
        system:
        let
          pkgs = self.packages.${system};
        in
        {
          domainchecker-server = {
            type = "app";
            program = "${pkgs.domainchecker-server}/bin/domainchecker-server";
          };
          domainchecker-client = {
            type = "app";
            program = "${pkgs.domainchecker-client}/bin/domainchecker-client";
          };
          default = self.apps.${system}.domainchecker-server;
        }
      );
    };
}
