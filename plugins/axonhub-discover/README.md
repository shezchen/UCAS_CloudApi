# axonhub-discover

OpenCode plugin that **auto-discovers models** from an [AxonHub](https://github.com/looplj/axonhub) (or any OpenAI-compatible) endpoint via `GET /v1/models`.

You no longer need to hand-maintain every model ID in `opencode.json` / `opencode.jsonc`.

## Why

OpenCode custom providers using `@ai-sdk/openai-compatible` require a `models` map. AxonHub already exposes a full model list at `/v1/models` (id, name, context, pricing, capabilities). This plugin fetches that list on startup and injects it into the provider config.

## Requirements

- [OpenCode](https://opencode.ai) with plugin support
- An AxonHub (or compatible) base URL ending with `/v1`
- API key available either via:
  - OpenCode auth (`/connect` → provider id, stored in `~/.local/share/opencode/auth.json`), or
  - `provider.<id>.options.apiKey` in config

## Install

### Option A — copy into your OpenCode config dir

```bash
mkdir -p ~/.config/opencode/plugins/axonhub-discover
cp index.ts ~/.config/opencode/plugins/axonhub-discover/
```

Then register the plugin (see [Config](#config)).

### Option B — reference this repo path

If you clone AxonHub locally, point `plugin` at this file:

```jsonc
"plugin": [
  ["/absolute/path/to/axonhub/plugins/axonhub-discover/index.ts", { "providerID": "axonhub" }]
]
```

### Option C — project-local

```bash
mkdir -p .opencode/plugin
cp plugins/axonhub-discover/index.ts .opencode/plugin/axonhub-discover.ts
```

Files under `.opencode/plugin/` are auto-loaded by OpenCode (no `plugin` entry required). You still need the `provider` block below.

## Config

Minimal `~/.config/opencode/opencode.jsonc`:

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "plugin": [
    ["./plugins/axonhub-discover/index.ts", { "providerID": "axonhub" }]
  ],
  "provider": {
    "axonhub": {
      "name": "AxonHub",
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "baseURL": "https://your-axonhub.example.com/v1"
      },
      // Optional: only overrides. Discovery fills the rest.
      "models": {
        "gpt-5.6-sol": {
          "variants": {
            "low": { "reasoningEffort": "low" },
            "medium": { "reasoningEffort": "medium" },
            "high": { "reasoningEffort": "high" }
          }
        }
      }
    }
  }
}
```

1. Run `/connect` in OpenCode, choose **Other**, provider id = `axonhub`, paste your AxonHub API key.  
   Or set `"apiKey": "ah-..."` under `options` (not recommended for shared configs).
2. Set `options.baseURL` to your AxonHub OpenAI-compatible endpoint (`.../v1`).
3. **Restart OpenCode** (plugins load at startup).
4. Open `/models` — discovered models should appear under the provider.

## Plugin options

| Option       | Default     | Description                                      |
| ------------ | ----------- | ------------------------------------------------ |
| `providerID` | `axonhub`   | Key under `provider` in OpenCode config          |
| `authID`     | `providerID`| Key in `auth.json` if different from provider id |
| `timeoutMs`  | `15000`     | HTTP timeout for `GET /models`                   |

Example with a custom provider id:

```jsonc
"plugin": [
  ["./plugins/axonhub-discover/index.ts", {
    "providerID": "my-gateway",
    "authID": "my-gateway",
    "timeoutMs": 20000
  }]
]
```

## Merge rules

- **Discovered** fields: `name`, `limit`, `modalities`, `reasoning`, `tool_call`, `attachment`, `cost`
- **Config wins** on conflict (e.g. your `variants`, custom `name`)
- Models only present in config (not returned by API) are **kept**
- If discovery fails (network, 401, timeout), existing config models are **unchanged**

## Mapped API fields

From AxonHub `GET /v1/models` item → OpenCode model config:

| API                         | OpenCode              |
| --------------------------- | --------------------- |
| `id`                        | model key             |
| `name`                      | `name`                |
| `context_length`            | `limit.context`       |
| `max_output_tokens`         | `limit.output`        |
| `modalities.input/output`   | `modalities`          |
| `capabilities.reasoning`    | `reasoning`           |
| `capabilities.tool_call`    | `tool_call`           |
| `capabilities.vision`       | `attachment: true`    |
| `pricing.*`                 | `cost.*`              |

## Troubleshooting

- **No new models**: restart OpenCode; confirm `providerID` matches config; check API key with  
  `curl -H "Authorization: Bearer $KEY" "$BASE_URL/models"`
- **401**: key missing or wrong `authID`; run `/connect` again
- **Wrong path**: `baseURL` must be the OpenAI root (`.../v1`), not `.../v1/models`
- **Variants missing**: keep them under `provider.<id>.models.<modelId>.variants` in config

## License

Same as the AxonHub repository.
