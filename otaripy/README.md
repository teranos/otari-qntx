# otaripy

QNTX's LLM provider with [otari](https://github.com/mozilla-ai/otari) inside it: [pyre](https://github.com/teranos/pyre) built with otari's Python environment, otari's app running in the plugin's own process. Nothing listens; the plugin is the gateway.

It serves, below `/api/otaripy` on the node:

| | |
|---|---|
| `LLMService` `Chat`, `StreamChat` | The node's LLM calls, answered with `served` (QNTX #1511). |
| `POST /v1/chat/completions` | OpenAI's chat completions. The body's `user` is who the node admitted: the asking token's DID, else the user, else `qntx-unattributed`. |
| `GET /usage?request_id=` | otari's record of one call, by the `Otari-Request-ID` its answer carried. |

The budgets are in [`python/otaripy/config.yml`](python/otaripy/config.yml): each asker's per day, everything through the node per month.

## Putting it on a node

Read on q, each step in the order it has to be taken.

1. **Add the record** by its tree URL, which names the plugin `otaripy` and the branch whose workflow runs the node installs from:
   `{"repo":"https://github.com/teranos/otari-qntx/tree/<branch>/otaripy"}` to `POST /api/plugins/check`, then `POST /api/plugins`.
2. **Set the config before enabling.** On a running plugin the node checks a config against pyre's schema and refuses every key pyre does not name. To change one later: disable, `PUT`, enable.
   ```json
   {"config": {
     "namespace": "Clean",
     "openrouter_api_key": "ssm:///q/box/openrouter-qntx/api-key",
     "data_dir": "/var/lib/qntx/otaripy",
     "site_dir": "/var/lib/qntx/otaripy-site"
   }}
   ```
   `namespace` is required: a record that names none is handed no token, its ATS query is refused, and the plugin starts with no handler and serves 404 (QNTX `plugin/grpc/config_provider.go`, `authToken`). `openrouter_api_key` is a reference the node resolves before the plugin starts; the key is written nowhere else.
3. **Attest the handler** from a session standing in that namespace (`GET /i/standing` says which): [`handlers/gateway.py`](handlers/gateway.py) as `{"subjects":["gateway"],"predicates":["handler"],"contexts":["otaripy"],"attributes":{"code":<the file>}}` to `POST /api/attestations`. The plugin reads its handlers from the store of the namespace it stands in, and nowhere else.
4. **Enable.** The plugin reads `@llm` and `@route` at Initialize. A handler attested on a running plugin reloads its routes, but whether the node sends it LLM calls is fixed at Initialize: disable and enable after one.
5. **The build** reaches the node as the artifact of a successful run of [`ci/otaripy.nix`](../ci/otaripy.nix) on that branch, for an enabled record; a run that finished before step 4 is run again (`gh run rerun`).

The first call starts otari, its migrations included: 34.8 s on q, where the next took 2.1 s.

## Config

| Key | |
|---|---|
| `namespace` | The namespace the plugin stands in (QNTX ADR-046). Required. |
| `openrouter_api_key` | The upstream key, as `ssm:///…` or `env:…`. Required. |
| `data_dir` | otari's database, and the master and service keys otaripy makes on first start (mode 0600). `/var/lib/qntx/otaripy` when unset. |
| `site_dir` | pyre's runtime site directory. |
| `timeout_seconds` | How long one call to otari may take before it is cancelled. 300 when unset. |
| `otari_config` | Another config.yml for otari; the in-process check uses one. |

## Measured on q

- The artifact: 125,889,078 bytes against the node's 30 s download; [`ci/size.sh`](ci/size.sh) holds it under 150,000,000.
- The plugin's resident memory: 32,752 kB at start, 474,980 kB after the first call, on a box of 1,936 MB.
  - Laptop measurements of the same imports: otari's `gateway.main` and what it imports reach 254.5 MB (4,482 modules; anthropic's SDK alone 1,252 of them, 44.3 MB; botocore 25.1 MB; numpy 14.2 MB, through any-guardrail's huggingface provider), and starting otari and serving a call brings the process to 368.8 MB.
  - pandas is installed and is not imported for a completion.
  - What is imported is otari's own eager import graph; loading less needs those imports made lazy in otari, any-llm and any-guardrail.

## Not yet

- `StreamChat` sends the whole answer as one final chunk.
- `/usage` reads any call's record with otari's master key, for whoever the node admits to the route.
