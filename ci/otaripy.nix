# The otaripy workflow. .github/workflows/otaripy.yml is emitted from this and
# is never edited by hand:
#
#   nix eval --json --file ci/otaripy.nix | jq . > .github/workflows/otaripy.yml
#
# JSON is YAML, so GitHub reads the emitted file as it is.
#
# Copied from capy's release recipe (ci/capy-release.nix at 68733c5): the
# build, package, store-path and run-on-the-target steps as capy has them,
# with capy's names made otaripy's and the import check asking for otari.
# What differs: no GitHub Release. QNTX installs a plugin from a successful
# workflow run's artifact in the repository and branch its record names
# (server/plugin_artifacts.go; release fetching went in ef63efc4b), so every
# successful run uploads the archive and its .sha256 as one artifact named
# after the archive, which is the name the node matches.
let
  version = "\${{ steps.version.outputs.version }}";
  artifact = "\${{ steps.package.outputs.artifact }}";
in
{
  name = "otaripy";

  on = {
    push = {
      branches = [ "otaripy-is-otari-inside-pyre" ];
      paths = [
        "otaripy/**"
        "ci/otaripy.nix"
        ".github/workflows/otaripy.yml"
      ];
    };
    workflow_dispatch = null;
  };

  permissions.contents = "read";

  # Debian 13 (trixie), what the box runs. Pinned by digest so the
  # portability check cannot drift under us.
  env.VERIFY_IMAGE = "debian@sha256:020c0d20b9880058cbe785a9db107156c3c75c2ac944a6aa7ab59f2add76a7bd";

  jobs.linux-amd64 = {
    runs-on = "ubuntu-latest";
    steps = [
      { uses = "actions/checkout@v5"; }

      {
        uses = "cachix/install-nix-action@v30";
        "with".extra_nix_config = ''
          experimental-features = nix-command flakes
        '';
      }

      # otaripy's module against otari in its own process and a stand-in
      # upstream: Chat with what served it, the gateway's end user, the usage
      # record. Not in capy's recipe; capy has no check of its own.
      {
        name = "In-process check";
        working-directory = "otaripy";
        run = "nix build .#checks.x86_64-linux.inprocess --show-trace -L";
      }

      {
        name = "Build plugin";
        working-directory = "otaripy";
        run = "nix build .#otaripy --show-trace -L";
      }

      # Asked of the built derivation, so it is the version in the artifact
      # and not a second copy of it.
      {
        name = "Resolve version";
        id = "version";
        working-directory = "otaripy";
        run = ''
          VERSION="$(nix eval --raw .#otaripy.version)"
          echo "version=$VERSION" >> "$GITHUB_OUTPUT"
        '';
      }

      # Both names are dictated by the node, not chosen: the archive must end
      # in -<GOOS>-<GOARCH>.tar.gz and the binary inside must be
      # qntx-<name>-plugin. See QNTX plugin/grpc/runner.go, BuildPlugin.
      {
        name = "Package";
        id = "package";
        working-directory = "otaripy";
        run = ''
          ARTIFACT="qntx-otaripy-plugin-${version}-linux-amd64.tar.gz"
          PATH="$(nix build nixpkgs#patchelf --no-link --print-out-paths)/bin:$PATH" \
            ci/package.sh result release
          tar -czf "$ARTIFACT" -C release qntx-otaripy-plugin qntx-otaripy-plugin.bin lib/ python/
          sha256sum "$ARTIFACT" > "$ARTIFACT.sha256"
          echo "artifact=$ARTIFACT" >> "$GITHUB_OUTPUT"
        '';
      }

      # A store path here means package.sh did not run or did not take.
      {
        name = "Verify no store paths remain";
        working-directory = "otaripy";
        run = ''
          BIN=release/qntx-otaripy-plugin.bin
          readelf -l "$BIN" | grep 'Requesting program interpreter'
          if readelf -l "$BIN" | grep -q '/nix/store'; then
            echo "artifact still names a /nix/store interpreter" >&2
            exit 1
          fi
          if readelf -d "$BIN" | grep -q '/nix/store'; then
            echo "artifact still has /nix/store in its RPATH" >&2
            exit 1
          fi
        '';
      }

      # Runs what ships, where it ships. Importing otari's app and otaripy is
      # the part a bare --version would not prove.
      {
        name = "Verify artifact runs on the target";
        working-directory = "otaripy";
        run = ''
          rm -rf /tmp/verify && mkdir -p /tmp/verify
          tar -xzf "${artifact}" -C /tmp/verify
          docker run --rm -v /tmp/verify:/plugin:ro "$VERIFY_IMAGE" \
            /plugin/qntx-otaripy-plugin --version

          # set -e, and the import is the last command, so its status is the
          # step's.
          docker run --rm -v /tmp/verify:/plugin:ro "$VERIFY_IMAGE" sh -ec '
            ls -l /plugin/python/bin/python3
            PYTHONHOME=/plugin/python /plugin/python/bin/python3 -c "import gateway.main, otaripy, httpx; print(otaripy.__file__)"
          '
        '';
      }

      {
        name = "Upload artifact";
        uses = "actions/upload-artifact@v4";
        "with" = {
          name = artifact;
          path = ''
            otaripy/${artifact}
            otaripy/${artifact}.sha256
          '';
        };
      }
    ];
  };
}
