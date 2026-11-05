"""otaripy against otari in its own process, as pyre runs it: otari started on
first use, on its loop thread, asked through the ASGI transport, with a
stand-in upstream on loopback. No network: it runs in the Nix sandbox.

argv: the stand-in upstream's script, a scratch directory.
"""

import json
import os
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

import otaripy

mock, scratch = sys.argv[1], Path(sys.argv[2])
upstream = subprocess.Popen([sys.executable, mock, "18080"])
for _ in range(100):
    try:
        urllib.request.urlopen("http://127.0.0.1:18080/v1/models", timeout=1)
        break
    except OSError:
        time.sleep(0.1)

os.environ["OTARI_PROVIDER_ALLOW_PRIVATE_HOSTS"] = "true"
config_yml = scratch / "otari.yml"
config_yml.write_text(
    """
providers:
  mock:
    provider_type: openai
    api_base: "http://127.0.0.1:18080/v1"
    api_key: ${OPENROUTER_API_KEY}
pricing:
  mock:good-model:
    input_price_per_million: 1.0
    output_price_per_million: 2.0
require_pricing: true
bootstrap_api_key: false
budgets:
  per-asker: {max_budget: 1, reset_alignment: calendar_day}
  qntx-ceiling: {max_budget: 30, reset_alignment: calendar_month}
api_keys:
  qntx:
    secret: ${OTARI_QNTX_KEY}
    user_id: qntx
    is_service_key: true
    end_user_budget_id: per-asker
    ceiling: qntx-ceiling
"""
)
config = {
    "openrouter_api_key": "sk-mock-upstream",
    "data_dir": str(scratch / "data"),
    "otari_config": str(config_yml),
}

try:
    # The node's Chat, as the @llm function answers it.
    answer = otaripy.chat(
        {"model": "mock:good-model", "provider": "otaripy",
         "messages": [{"role": "user", "content": "hi"}]},
        config,
    )
    print("chat:", json.dumps(answer))
    assert "error" not in answer, answer
    assert answer["content"], answer
    served = answer["served"]
    assert served["request_id"], served
    assert served["upstream_provider"] == "mock", served
    assert served["requested_model"] == "mock:good-model", served
    assert served.get("cost_usd"), served

    # A call naming no model is refused, not given one.
    refused = otaripy.chat({"messages": [{"role": "user", "content": "hi"}]}, config)
    assert refused["error"]["code"] == "INVALID_ARGUMENT", refused

    # The gateway: who the node admitted is otari's end user.
    asker = "did:key:z6Mke2eCaller"
    passed = otaripy.completions(
        {"method": "POST", "path": "/v1/chat/completions", "query": "",
         "headers": {"x-qntx-asker-did": asker, "content-type": "application/json"},
         "body": json.dumps({"model": "mock:good-model", "user": "someone-else",
                             "messages": [{"role": "user", "content": "hi"}]}).encode()},
        config,
    )
    print("completions:", passed["status"], {k: v for k, v in passed["headers"].items()})
    assert passed["status"] == 200, passed
    said = {k.lower(): v for k, v in passed["headers"].items()}
    # otari took the asker as an end user of the service key, on the budget
    # the key starts its end users on.
    assert said["otari-end-user-budget"] == "per-asker", said
    request_id = said["otari-request-id"]

    # otari's record of that call, by its Otari-Request-ID.
    record = otaripy.usage({"query": "request_id=" + request_id}, config)
    print("usage:", record["status"], record["body"].decode())
    assert record["status"] == 200, record
    assert json.loads(record["body"])["status"] == "success", record

    # The keys otaripy keeps: made once, readable by the plugin's user alone.
    for kept in ("master-key", "service-key"):
        mode = (scratch / "data" / kept).stat().st_mode & 0o777
        assert mode == 0o600, (kept, oct(mode))
    print("otaripy in process: every check held")
finally:
    upstream.terminate()
