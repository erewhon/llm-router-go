# Running the router on macOS (or any workstation)

The `router` is a self-contained, OpenAI-compatible front door. It reads one
`models.yaml`, forwards `/v1/*` requests to whatever backends you configure,
and streams responses back. It needs **no** database, tool proxy, or node
agents — those are all optional. This makes it a good fit for running in the
foreground on a laptop to unify, say, **Amazon Bedrock** and a **local LM
Studio** model behind a single endpoint.

## Install

```sh
brew tap erewhon/tap
brew install llm-router
```

Upgrade later with `brew upgrade llm-router`.

## Configure

Start from the bundled example, which documents every backend type:

```sh
mkdir -p ~/.config/llm-router
cp "$(brew --prefix)/share/llm-router/models.example.yaml" ~/.config/llm-router/models.yaml
$EDITOR ~/.config/llm-router/models.yaml
```

A minimal config with just the two targets below is all you need — you can
delete the `nodes:` block and any scenarios you don't use.

### Amazon Bedrock (Anthropic Claude)

Claude over the OpenAI Chat Completions format is served by Bedrock's
**`bedrock-mantle`** endpoint. Authentication is a **Bedrock API key** used as a
bearer token.

1. Create a Bedrock API key in the AWS console (Amazon Bedrock → API keys) and
   export it. The router reads it from the environment, so it never lives in the
   config file:

   ```sh
   export AWS_BEARER_TOKEN_BEDROCK="<your Bedrock API key>"
   ```

2. Add a model. `api_key` holds the **name** of the env var (any value not
   starting with `sk-` is treated as an env var name):

   ```yaml
   bedrock-claude:
     hf_repo: anthropic.claude-sonnet-4-5   # confirm exact id — see below
     backend: external
     api_base: https://bedrock-mantle.us-east-1.api.aws/v1   # pick your region
     api_key: AWS_BEARER_TOKEN_BEDROCK
     aliases: [claude, sonnet]
     capabilities: [text, tool_calling]
   ```

   Confirm the exact model id available to you (ids differ by region/account):

   ```sh
   curl https://bedrock-mantle.us-east-1.api.aws/v1/models \
        -H "Authorization: Bearer $AWS_BEARER_TOKEN_BEDROCK"
   ```

   Regional endpoints follow `https://bedrock-mantle.<region>.api.aws/v1`
   (e.g. `us-east-1`, `us-west-2`, `eu-central-1`).

### LM Studio (local model on this Mac)

Enable LM Studio's local server (it listens on `http://localhost:1234/v1`).
`hf_repo` is the model identifier LM Studio shows for the loaded model. No API
key is required:

```yaml
local-lmstudio:
  hf_repo: qwen2.5-coder-7b-instruct
  backend: external
  api_base: http://localhost:1234/v1
  aliases: [local, lmstudio]
  capabilities: [text, tool_calling]
```

## Falling back to the local model

A **role** is a name that resolves to the first usable entry in a list. Put the
gateway's models first and the model on this Mac last, and callers that ask for
the role keep working when the gateway refuses:

```yaml
roles:
  coder:
    require: {capabilities: [text, tool_calling]}
    candidates: [gateway-glm, gateway-qwen, local-lmstudio]
```

A request moves to the next candidate when the one tried does not answer, or
answers **5xx** or **429**. Any other 4xx is returned as it is: the request is
at fault and would fail the same way on the next seat. One request tries at
most three candidates. The last one tried is never retried past, so its own
status and body, `Retry-After` included, reach the caller.

A seat that keeps failing is skipped without being asked: three failures in a
row take it out for 60 s, then one request is let through to test it. A 429 or
503 that carries `Retry-After` takes the seat out at once, for as long as it
asked, up to 10 minutes. `GET /v1/availability` shows each role's current
target and why any candidate is out.

Only a role or a chain does this. A request that names a model gets that model
or its error. Leave a planning or review model out of any such list, or give it
a role with one candidate: a small model answering in its place is worse than
an error.

## Run (foreground)

```sh
llm-router -models-yaml ~/.config/llm-router/models.yaml -addr :4010
```

Useful flags (all optional):

| Flag | Purpose |
| --- | --- |
| `-addr :4010` | Listen address (default `:4010`; `127.0.0.1:4010` keeps it off the network). |
| `-api-keys sk-abc,sk-def` | Require a bearer token on `/v1/*`. Omit to allow any local caller (also settable via `$ROUTER_API_KEYS`). |
| `-log-format text` | Human-readable logs instead of JSON. |
| `-mode <tag>` | Filter to models tagged `mode:<tag>` (plus untagged). |
| `-version` | Print version and exit. |

The Postgres request log and tool proxy stay off unless you pass
`-postgres-dsn` / `-tool-proxy-url`, so nothing else needs to be running.

## Smoke test

```sh
# List configured models
curl -s localhost:4010/v1/models | jq '.data[].id'

# Call Bedrock Claude through the router
curl -s localhost:4010/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"claude","messages":[{"role":"user","content":"Say hi in five words."}]}' | jq

# Call the local LM Studio model
curl -s localhost:4010/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"local","messages":[{"role":"user","content":"Say hi."}]}' | jq
```

You can request a model by its registry key (`bedrock-claude`) or any of its
aliases (`claude`, `sonnet`, `local`, …).

## Notes

- **Streaming** (`"stream": true`) is passed through untouched.
- The router forwards the OpenAI request body as-is, rewriting only the `model`
  field to the backend's `hf_repo`. Backend-specific parameters you include in
  the request body reach the backend unchanged.
- Bedrock's `bedrock-mantle` endpoint does not support AWS Guardrails or
  cross-region inference profiles; use `bedrock-runtime` (a different API) if you
  need those. For plain chat, `bedrock-mantle` is the right choice.

## See also

- [Local Orpheus TTS on macOS](orpheus-say-macos.md) — run the `orpheus-say`
  text-to-speech CLI against a local mlx-audio Orpheus server on the same laptop.
