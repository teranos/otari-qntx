{
  description = "otaripy — pyre with otari baked in: QNTX's LLM provider, otari in process";

  inputs = {
    # The release whose C library is no newer than the box's: q runs Debian 13
    # with glibc 2.41, and the tree carries none of its own, so every ELF in
    # it is built against 2.40 here. nixos-unstable's 2.44 does not load
    # there (ci/glibc.sh says which file asks for what).
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";
    flake-utils.url = "github:numtide/flake-utils";
    # pyre with @llm, @route and plugin_config (teranos/pyre#6).
    pyre-src = {
      url = "github:teranos/pyre/llm-handlers-make-pyre-a-provider";
      flake = false;
    };
    # The protocol pyre builds against: QNTX llm-served (#1511), the ref
    # pyre's Cargo.lock names, which carries LLMServed.
    qntx-src = {
      url = "github:teranos/QNTX/035c62ed46375dde51243efe509006e610a8a76e";
      flake = false;
    };
    # otari at the commit the plugin's e2e check runs against, with
    # patches/otari-package.patch (its unmerged nix-flake change) applied.
    otari = {
      url = "github:mozilla-ai/otari/ca0231e6e1694c6cee8bf7467779183b48739981";
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
      flake-utils,
      pyre-src,
      qntx-src,
      otari,
      pyproject-nix,
      uv2nix,
      pyproject-build-systems,
    }:
    flake-utils.lib.eachSystem [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ] (
      system:
      let
        pkgs = import nixpkgs { inherit system; };
        inherit (pkgs) lib;

        # The version. Compiled in, reported by the running plugin, and the
        # name the workflow's artifact is published under.
        otaripyVersion = "0.1.0";

        otariSrc = pkgs.applyPatches {
          name = "otari-src-nix";
          src = otari;
          patches = [ ./patches/otari-package.patch ];
        };

        otariSet =
          (import "${otariSrc}/nix/package.nix" {
            inherit
              lib
              pyproject-nix
              uv2nix
              pyproject-build-systems
              ;
            src = otariSrc;
            version = "0.35.0-unstable+${builtins.substring 0 8 otari.rev}";
          }).forPkgs
            { inherit pkgs; };

        # otaripy's own module, the config otari starts with, and otari's
        # migrations beside them: the migrations live outside the gateway
        # wheel, and the module tells otari where they are.
        otaripyModule = pkgs.runCommand "otaripy-module-${otaripyVersion}" { } ''
          site=$out/${pkgs.python313.sitePackages}/otaripy
          mkdir -p $site
          cp ${./python/otaripy}/*.py ${./python/otaripy}/config.yml $site/
          cp -r ${otariSet.otari-migrations}/share/otari/alembic $site/alembic
          cp ${otariSet.otari-migrations}/share/otari/alembic.ini $site/alembic.ini
        '';

        # One tree with the interpreter's standard library, otari's locked
        # dependencies and otaripy's module, as capy's withPackages is one.
        # otari's venv holds only site-packages, and the release tree runs on
        # PYTHONHOME, which needs the standard library beside them.
        # python313 first, so where both hold a file (bin/python3 among
        # them) the interpreter's own is the one joined; and the venv's
        # pyvenv.cfg left out. Both name python313 by its store path.
        pythonEnv = pkgs.symlinkJoin {
          name = "otaripy-env";
          paths = [
            pkgs.python313
            otariSet.otari-venv
            otaripyModule
          ];
          postBuild = ''
            rm $out/pyvenv.cfg
          '';
        };

        outputHashes = {
          "qntx-ats-0.2.44" = "sha256-xFz5NDwDLEr7t2eG1K2PFvJP17ke00NZ2VlLwh4YKgE=";
        };

        otaripy = pkgs.rustPlatform.buildRustPackage {
          pname = "qntx-otaripy-plugin";
          version = otaripyVersion;
          src = pyre-src;

          QNTX_PLUGIN_VERSION = otaripyVersion;

          cargoLock = {
            lockFile = "${pyre-src}/Cargo.lock";
            inherit outputHashes;
          };

          buildInputs = with pkgs; [
            protobuf
            pythonEnv
            openssl
          ];

          nativeBuildInputs = with pkgs; [
            pkg-config
            protobuf
            makeWrapper
          ];

          QNTX_PROTO_DIR = "${qntx-src}/plugin/grpc/protocol";
          PYO3_PYTHON = "${pythonEnv}/bin/python3";

          # pyre's own tests run in the check phase, against otari's
          # interpreter; the DIRE ones reach PyPI and stay ignored there.
          postInstall = ''
            mv $out/bin/pyre $out/bin/qntx-otaripy-plugin
          ''
          + lib.optionalString pkgs.stdenv.isLinux ''
            patchelf --set-rpath "${lib.makeLibraryPath [ pythonEnv ]}:$(patchelf --print-rpath $out/bin/qntx-otaripy-plugin)" \
              $out/bin/qntx-otaripy-plugin
          ''
          + lib.optionalString pkgs.stdenv.isDarwin ''
            install_name_tool -add_rpath "${lib.makeLibraryPath [ pythonEnv ]}" \
              $out/bin/qntx-otaripy-plugin
          ''
          + ''
            wrapProgram $out/bin/qntx-otaripy-plugin \
              --prefix PYTHONPATH : "${pythonEnv}/${pkgs.python313.sitePackages}" \
              --add-flags "--name otaripy"
          '';
        };
      in
      {
        packages = {
          default = otaripy;
          inherit otaripy;
          python = pythonEnv;
          # The in-process check and its stand-in upstream, for the workflow
          # to run against the packaged tree on the target image.
          inprocess-files = pkgs.runCommand "otaripy-inprocess-files" { } ''
            mkdir -p $out
            cp ${./tests/inprocess.py} $out/inprocess.py
            cp ${otariSrc}/nix/tests/mock_openai.py $out/mock_openai.py
          '';
        };

        # The interpreter the release tree carries is python313's own, not the
        # venv's launcher: that launcher execs python313 by its store path,
        # which a box without /nix lacks, and it dies with 255 and no word
        # (run 41956638205, the step that runs the artifact on Debian). And
        # no pyvenv.cfg, whose home names that store path too.
        checks.interpreter = pkgs.runCommand "otaripy-interpreter" { } ''
          ran=$(readlink -f ${pythonEnv}/bin/python3)
          own=$(readlink -f ${pkgs.python313}/bin/python3)
          echo "bin/python3 is $ran; python313's is $own"
          [ "$ran" = "$own" ]
          if [ -e ${pythonEnv}/pyvenv.cfg ]; then
            echo "the env carries pyvenv.cfg: $(cat ${pythonEnv}/pyvenv.cfg)"
            exit 1
          fi
          PYTHONPATH=${pythonEnv}/${pkgs.python313.sitePackages} ${pythonEnv}/bin/python3 -c 'import gateway.main, otaripy' > $out
        '';

        # otaripy's module against otari in its own process and a stand-in
        # upstream on loopback, in the sandbox: Chat with what served it, the
        # gateway naming the asker otari's end user, and otari's usage record.
        checks.inprocess = pkgs.runCommand "otaripy-inprocess" { } ''
          export HOME=$TMPDIR
          # The sandbox names /no-cert-file.crt, and httpx opens it whatever
          # the call dials.
          export SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt
          # As pyre's wrapper sets it: the interpreter resolves to otari's
          # venv, whose own site-packages hold otari and not otaripy.
          export PYTHONPATH=${pythonEnv}/${pkgs.python313.sitePackages}
          ${pythonEnv}/bin/python3 ${./tests/inprocess.py} \
            ${otariSrc}/nix/tests/mock_openai.py $TMPDIR | tee $out
          grep -q 'every check held' $out
        '';

        apps.default = {
          type = "app";
          program = "${otaripy}/bin/qntx-otaripy-plugin";
        };
      }
    );
}
