{
  description = "qntx-otari-plugin: QNTX's LLM provider (ADR-014) backed by otari";

  inputs = {
    # The nixpkgs QNTX takes Go 1.26 from (QNTX flake.nix, nixpkgs-go).
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

    # QNTX's plugin protocol is QNTX's source (plugin/grpc/protocol, plugin/grpc),
    # as gaze and scry take it. Pinned in flake.lock;
    # patches/qntx-llm-served-protocol.patch is the part of QNTX's llm-served
    # change this plugin builds against (LLMServed, LLMServiceProvider), applied
    # until that change is merged there.
    qntx = {
      url = "github:teranos/QNTX";
      flake = false;
    };

    # otari, for the end-to-end check: its source at the pinned commit, with
    # patches/otari-package.patch (from otari's nix-flake change: the migrations
    # fix and nix/package.nix) applied until that is merged there, built from
    # its uv.lock by uv2nix.
    otari = {
      url = "github:mozilla-ai/otari";
      flake = false;
    };
    pyproject-nix = {
      url = "github:pyproject-nix/pyproject.nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    uv2nix = {
      url = "github:pyproject-nix/uv2nix";
      inputs.pyproject-nix.follows = "pyproject-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    pyproject-build-systems = {
      url = "github:pyproject-nix/build-system-pkgs";
      inputs.pyproject-nix.follows = "pyproject-nix";
      inputs.uv2nix.follows = "uv2nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      qntx,
      otari,
      pyproject-nix,
      uv2nix,
      pyproject-build-systems,
    }:
    let
      inherit (nixpkgs) lib;
      # nixpkgs 26.11 has dropped x86_64-darwin.
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      forAll = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      qntxSrcFor =
        pkgs:
        pkgs.applyPatches {
          name = "qntx-src-llm-served";
          src = qntx;
          patches = [ ./patches/qntx-llm-served-protocol.patch ];
        };

      otariSrcFor =
        pkgs:
        pkgs.applyPatches {
          name = "otari-src-nix";
          src = otari;
          patches = [ ./patches/otari-package.patch ];
        };

      otariFor =
        pkgs:
        let
          src = otariSrcFor pkgs;
          set = import "${src}/nix/package.nix" {
            inherit
              lib
              pyproject-nix
              uv2nix
              pyproject-build-systems
              ;
            inherit src;
            version = "0.35.0-unstable+${builtins.substring 0 8 otari.rev}";
          };
        in
        (set.forPkgs { inherit pkgs; }).otari;

      pluginFor =
        pkgs:
        let
          qntxSrc = qntxSrcFor pkgs;
        in
        pkgs.buildGoModule {
          pname = "qntx-otari-plugin";
          version = "0.2.0";
          src = lib.fileset.toSource {
            root = ./.;
            fileset = lib.fileset.unions [
              ./go.mod
              ./go.sum
              ./cmd
              ./otari
              ./plugin
              ./e2e
            ];
          };
          # go.mod replaces github.com/teranos/QNTX with ./qntx.
          postPatch = ''
            rm -rf qntx
            cp -r ${qntxSrc} qntx
            chmod -R u+w qntx
          '';
          # The module cache (go mod download), not a vendor directory: `go mod
          # vendor` resolves the embeds of every package under every build tag,
          # and QNTX's ats/wasm embeds a build artifact (ats.wasm) that only its
          # qntxwasm builds import, never this plugin.
          proxyVendor = true;
          vendorHash = "sha256-fDOP6u9NZCORMoUnOHG9btK/coMqCwnxqR/ak2ZrPjs=";
          subPackages = [ "cmd/qntx-otari-plugin" ];
          # A pure-Go binary: without cgo, QNTX's plugin packages leave out
          # the Rust-backed SQLite store, which a plugin never opens.
          env.CGO_ENABLED = 0;
          ldflags = [
            "-s"
            "-w"
            "-X github.com/teranos/otari-qntx/plugin.Version=0.2.0"
          ];
          # The unit tests (subPackages would test only cmd/); e2e/ is behind
          # its build tag and runs in checks.e2e.
          checkPhase = ''
            runHook preCheck
            go test -count=1 ./otari/... ./plugin/...
            runHook postCheck
          '';
          passthru = { inherit qntxSrc; };
          meta = {
            description = "QNTX LLM provider plugin backed by otari";
            mainProgram = "qntx-otari-plugin";
            license = lib.licenses.asl20;
          };
        };
    in
    {
      packages = forAll (pkgs: {
        default = pluginFor pkgs;
        qntx-otari-plugin = pluginFor pkgs;
      });

      apps = forAll (pkgs: {
        default = {
          type = "app";
          program = "${pluginFor pkgs}/bin/qntx-otari-plugin";
          meta.description = "The plugin binary, as the node runs it";
        };
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gopls
            (otariFor pkgs)
          ];
          env.CGO_ENABLED = "0";
          # go.mod's ./qntx: the pinned, patched QNTX, unless a checkout is there.
          shellHook = ''
            [ -e qntx ] || ln -s ${qntxSrcFor pkgs} qntx
          '';
        };
      });

      checks = lib.genAttrs [ "x86_64-linux" "aarch64-linux" ] (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          plugin = pluginFor pkgs;
        in
        {
          inherit plugin;
          # The built binary, driven as the node drives it, against otari
          # (built from its lock) and a stand-in upstream, in the sandbox.
          e2e = import ./nix/e2e.nix {
            inherit pkgs plugin;
            otari = otariFor pkgs;
            otariSrc = otariSrcFor pkgs;
          };
        }
      );
    };
}
