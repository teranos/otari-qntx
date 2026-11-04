# otari-qntx

QNTX's LLM provider ([ADR-014](https://github.com/teranos/QNTX/blob/main/docs/adr/ADR-014-llm-as-plugin-provided-service.md)) backed by [otari](https://github.com/mozilla-ai/otari), Mozilla AI's OpenAI-compatible gateway. One plugin, `otari`, in place of three:

| Was | Now |
|---|---|
| [openrouter-qntx](https://github.com/sbvh-nl/openrouter-qntx): hosted models through OpenRouter | an otari provider instance per hosted provider (`openai`, `anthropic`, `openrouter`, …), routing policies, budgets, pricing |
| [gaze](https://github.com/teranos/gaze): local llama.cpp inference | llama.cpp's own `llama-server` (or ollama, vLLM, LM Studio) as an otari instance (`provider_type: llamacpp` / `openai`) |
| [scry](https://github.com/teranos/scry): local inference with per-token signal | the same local backend; **otari reports no per-token signal**, so every chunk says `no_signal` (see Limits) |

The node calls `services.LLM()` / `/api/prompt/direct` with `provider: otari`; the plugin serves QNTX's `LLMService` (`plugin/grpc/protocol/llm.proto`: `Chat` and `StreamChat`) by calling otari's `POST /api/v1/chat/completions`, and answers with what otari reported of how it served the call.

## Build

```
nix build            # result/bin/qntx-otari-plugin, a static pure-Go binary
nix flake check      # unit tests, and the e2e check against otari in the sandbox
nix develop          # Go, and otari itself; links ./qntx (go.mod's replace) to the pinned QNTX
```

QNTX's protocol is QNTX's source: `flake.nix` takes QNTX as an input pinned in `flake.lock` and puts it at `./qntx`, which `go.mod` replaces `github.com/teranos/QNTX` with (as gaze and scry take it as `QNTX_SRC`). The plugin needs a QNTX change that is not merged yet (branch `llm-served`): `LLMServed` in `llm.proto`, carried through the node, and `LLMServiceProvider`, which lets a Go plugin serve the whole `LLMService` (streaming, `messages`, `served`), not only `plugin.LLMProvider`'s single-turn `Chat`. Until it is merged, `patches/qntx-llm-served-protocol.patch` applies the part of it the plugin builds against (the protocol and the plugin framework) to the pinned QNTX. Likewise `patches/otari-package.patch` is the part of otari's unmerged `nix-flake` change the e2e check builds otari with (the migrations fix and `nix/package.nix`). Reading the patched otari's `uv.lock` is import-from-derivation, so `nix flake check --no-build` cannot evaluate the e2e check and the dev shell; `nix flake check` can. Once both changes are merged, the patches go, `qntx` follows QNTX's main and `otari` becomes a flake input (`otari.packages.${system}.otari`).

Pinned (flake.lock): QNTX `84bbc37cfb6cc18668cac1320c23cc08b8244ce4`, otari `ca0231e6e1694c6cee8bf7467779183b48739981`, nixpkgs `0bb2eedc20e4739f29e8045034f2828ad7054f8f`.

## Configuration

The plugin is its record on the node ([ADR-002](https://github.com/teranos/QNTX/blob/main/docs/adr/ADR-002-plugin-configuration.md)): `plugins_add https://github.com/teranos/otari-qntx/tree/plugin/otari`, `PUT /api/plugins/otari/config` with the keys below, `plugins_enable`. The record's name is the URL's last segment, and the node reads `qntx-<that name>-plugin` back from the built binary's `--version`: the tree URL ending in `otari` names it `otari`, where the repository URL alone would name it `otari-qntx` and the build would be refused. Its `build.*` keys: `build.core` `github.com/teranos/otari-qntx@plugin`, `build.command` `nix build`, `build.output` `result/bin/qntx-otari-plugin`. `.github/workflows/ci.yml` is the checks the node waits on before it builds a pushed commit.

| Key | |
|---|---|
| `otari_url` | Where otari serves, e.g. `http://127.0.0.1:8000`. Required. |
| `api_key_file` | A file holding the otari API key the node calls with. Without it, `OTARI_API_KEY` from the plugin's environment. The key is never in the plugin's config record. |
| `timeout_seconds` | How long one call, its stream included, may take. 300 when unset. |

Mint the key with otari's master key: `curl -X POST $OTARI/api/v1/keys -H "Authorization: Bearer $OTARI_MASTER_KEY" -d '{"key_name":"qntx"}'`. Run otari itself with its flake's NixOS module (`services.otari`).

## Contract

- **The caller chooses.** `LLMChatRequest.model` goes to otari as its `model`, unaltered: a model (`openai:gpt-5-mini`), an alias, or a routing policy (`fast`). A call naming no model is refused (`INVALID_ARGUMENT`); the plugin picks none.
- **`messages` first.** When `messages` is set it is sent; otherwise the deprecated `system_prompt` / `user_prompt`. Attachments ride the last user message as OpenAI content parts (`image_url`, or `file` with the data URI and filename), which otari's content normalizer extracts or inlines per model.
- **Zero is unset.** proto3 gives `temperature` and `max_tokens` no presence, so 0 is not sent and otari's upstream default applies, as gaze and scry read them. `priority` is the node's queue's (`LLMServer`), not otari's.
- **What was asked stays apart from what served it.** The response, and the final chunk of a stream, carry `LLMServed`:

| `LLMServed` | from otari |
|---|---|
| `requested_provider` | `LLMChatRequest.provider` as received (`otari`) |
| `requested_model` | `LLMChatRequest.model` as received |
| `upstream_provider` | `Otari-Provider`: the instance that served it, by its configured name |
| `executed_model` | `Otari-Model`: the model that instance was sent |
| `response_model` | the body's `model`: the upstream's own name (`good-model-2026-01-01`) for a model asked for directly, the policy name for a routed one |
| `request_id` | `Otari-Request-ID`; otari's record of the call is `GET /api/v1/usage/requests/{id}` |
| `cost_usd` | `usage.cost_usd`, the decimal text otari wrote, never re-rendered through a float; absent when it reported none |
| `pricing_source` | `usage.pricing_source` |
| `attempt_count`, `fallback` | `Otari-Attempt-Count`, `Otari-Fallback` |

  `LLMChatResponse.model` (and the final chunk's) is `executed_model`, the model that ran, so the node's weave is `model:<what ran>`; when otari leaves `Otari-Model` out (a name a header cannot carry) it is the body's model, the only name otari reported. Token counts are otari's `usage`.
- **otari's refusals keep their words.** Its status maps to a gRPC code (400 → `INVALID_ARGUMENT`, 401 → `UNAUTHENTICATED`, 402/424 → `FAILED_PRECONDITION`, 403 → `PERMISSION_DENIED`, 404 → `NOT_FOUND`, 429 → `RESOURCE_EXHAUSTED`, 5xx → `UNAVAILABLE`, an error inside a begun stream → `ABORTED`); the message keeps otari's status, `Otari-Error-Code`, detail and `Otari-Request-ID`.

All of the above was read from otari's source at the pinned commit and checked against a running otari: `e2e/` drives the built binary as the node does (Initialize, then `Chat` and `StreamChat`) through a routing policy that falls over, and compares the cost and tokens reported with otari's own usage record for the request id. QNTX's `plugin/grpc/services/llm_served_e2e_test.go` runs the node's `LLMServer` and `GRPCLLMClient` against the running plugin.

## Limits

- **No per-token signal.** otari streams text deltas; it has no logits, so scry's confidence, entropy, top-k and sampler stages have no source here. Every chunk says `no_signal`. A deployment that needs them keeps scry for that.
- **One chunk per delta.** A chunk is what otari's upstream sent as one delta (often a word or a few tokens), not one token.
- **Text only back.** Tool calls, reasoning content and images an upstream returns have no field in `LLMChatResponse`; only the text is returned.
- **gaze's `/v1/responses`** is otari's own `POST /api/v1/responses`: point codex at otari directly rather than through the node.
