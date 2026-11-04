# The plugin binary, driven as the node drives it (e2e/e2e_test.go), against
# otari built from its lock, with otari's stand-in upstream on loopback. No
# network: everything runs in the build sandbox.
{
  pkgs,
  plugin,
  otari,
  otariSrc,
}:
pkgs.stdenv.mkDerivation {
  name = "qntx-otari-plugin-e2e";
  inherit (plugin) src postPatch;
  nativeBuildInputs = [
    pkgs.go
    pkgs.curl
    pkgs.jq
    pkgs.python3
  ];
  env.CGO_ENABLED = "0";
  dontConfigure = true;
  dontInstall = true;
  buildPhase = ''
    runHook preBuild
    export HOME=$TMPDIR GOCACHE=$TMPDIR/go-cache GOFLAGS=-mod=mod GOSUMDB=off
    export GOPROXY=file://${plugin.goModules}
    export SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt

    python3 ${otariSrc}/nix/tests/mock_openai.py 18080 > $TMPDIR/mock.log 2>&1 &
    mock=$!

    cat > $TMPDIR/otari.yml <<EOF
    providers:
      primary:
        provider_type: openai
        api_base: "http://127.0.0.1:18080/v1"
        api_key: \''${MOCK_UPSTREAM_KEY}
      secondary:
        provider_type: openai
        api_base: "http://127.0.0.1:18080/v1"
        api_key: \''${MOCK_UPSTREAM_KEY}
    pricing:
      primary:broken-model:
        input_price_per_million: 3.0
        output_price_per_million: 4.0
      secondary:good-model:
        input_price_per_million: 1.0
        output_price_per_million: 2.0
    routing:
      enabled: true
      policies:
        fast:
          select:
            - default: primary:broken-model
          on_failure:
            - secondary:good-model
    bootstrap_api_key: false
    budgets:
      e2e-end-user: {max_budget: 1.0, reset_alignment: calendar_day}
      e2e-spent: {max_budget: 0.000001, reset_alignment: calendar_day}
      e2e-ceiling: {max_budget: 100, reset_alignment: calendar_day}
    api_keys:
      qntx-gateway:
        secret: \''${E2E_SERVICE_KEY}
        user_id: qntx-gateway
        is_service_key: true
        end_user_budget_ids: [e2e-end-user, e2e-spent]
        end_user_budget_id: e2e-end-user
        ceiling: e2e-ceiling
    EOF

    export MOCK_UPSTREAM_KEY=sk-mock-upstream OTARI_MASTER_KEY=e2e-master-key-0123456789abcdef
    # The service key the gateway test calls with: a secret of the form otari mints.
    export E2E_SERVICE_KEY=$(python3 -c "import secrets; print('tk-' + secrets.token_urlsafe(48))")
    printf %s "$E2E_SERVICE_KEY" > $TMPDIR/otari-service-key
    export OTARI_PROVIDER_ALLOW_PRIVATE_HOSTS=true
    ${otari}/bin/otari serve --config $TMPDIR/otari.yml --database-url "sqlite:///$TMPDIR/otari.db" \
      --host 127.0.0.1 --port 18000 > $TMPDIR/otari.log 2>&1 &
    otari=$!
    for _ in $(seq 1 180); do
      curl -fsS http://127.0.0.1:18000/api/v1/health > /dev/null 2>&1 && break
      kill -0 $otari || { cat $TMPDIR/otari.log; exit 1; }
      sleep 1
    done

    curl -fsS -X POST http://127.0.0.1:18000/api/v1/keys -H "Authorization: Bearer $OTARI_MASTER_KEY" \
      -H 'Content-Type: application/json' -d '{"key_name":"qntx"}' | jq -er .key > $TMPDIR/otari-key

    OTARI_E2E_PLUGIN_BIN=${plugin}/bin/qntx-otari-plugin \
    OTARI_E2E_URL=http://127.0.0.1:18000 \
    OTARI_E2E_KEY_FILE=$TMPDIR/otari-key \
    OTARI_E2E_POLICY=fast \
    OTARI_E2E_PROVIDER=secondary \
    OTARI_E2E_MODEL=good-model \
    OTARI_E2E_COST=0.000023 \
    OTARI_E2E_SERVICE_KEY_FILE=$TMPDIR/otari-service-key \
    OTARI_E2E_SPENT_BUDGET=e2e-spent \
      go test -tags e2e -count=1 -v ./e2e/ 2>&1 | tee $TMPDIR/e2e.log
    test ''${PIPESTATUS[0]} -eq 0 || { cat $TMPDIR/otari.log; exit 1; }
    grep -q -- '--- PASS: TestE2E_TheNodesCallsThroughThePluginToOtari' $TMPDIR/e2e.log
    grep -q -- '--- PASS: TestE2E_TheGatewayBillsWhoTheNodeAdmitted' $TMPDIR/e2e.log

    kill $otari $mock
    mkdir -p $out
    cp $TMPDIR/e2e.log $out/
    runHook postBuild
  '';
}
