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

## Run (foreground)

```sh
llm-router -models-yaml ~/.config/llm-router/models.yaml -addr :4010
```

Useful flags (all optional):

| Flag | Purpose |
| --- | --- |
| `-addr :4010` | Listen address (default `:4015`). |
| `-api-keys sk-abc,sk-def` | Require a bearer token on `/v1/*`. Omit to allow any local caller (also settable via `$ROUTER_API_KEYS`). |
| `-log-format text` | Human-readable logs instead of JSON. |
| `-mode big` | Filter to models tagged `mode:big` (plus untagged). |
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
