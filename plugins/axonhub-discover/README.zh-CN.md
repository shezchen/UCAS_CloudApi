# axonhub-discover

OpenCode 插件：从 [AxonHub](https://github.com/looplj/axonhub)（或任意 OpenAI 兼容网关）的 `GET /v1/models` **自动发现模型列表**。

不必再在 `opencode.json` / `opencode.jsonc` 里手写每一个 model id。

## 为什么需要它

OpenCode 自定义供应商（`@ai-sdk/openai-compatible`）要求配置 `models` 映射。AxonHub 已在 `/v1/models` 返回完整列表（id、名称、上下文、定价、能力）。本插件在启动时拉取并注入到 provider 配置中。

## 环境要求

- 支持 plugin 的 [OpenCode](https://opencode.ai)
- AxonHub（或兼容）地址，以 `/v1` 结尾
- API Key，来源二选一：
  - OpenCode `/connect`（写入 `~/.local/share/opencode/auth.json`）
  - 配置里的 `provider.<id>.options.apiKey`

## 安装

### 方式 A — 复制到 OpenCode 配置目录

```bash
mkdir -p ~/.config/opencode/plugins/axonhub-discover
cp index.ts ~/.config/opencode/plugins/axonhub-discover/
```

然后按下方 [配置](#配置) 注册插件。

### 方式 B — 直接引用本仓库路径

```jsonc
"plugin": [
  ["/绝对路径/axonhub/plugins/axonhub-discover/index.ts", { "providerID": "axonhub" }]
]
```

### 方式 C — 项目本地自动加载

```bash
mkdir -p .opencode/plugin
cp plugins/axonhub-discover/index.ts .opencode/plugin/axonhub-discover.ts
```

`.opencode/plugin/` 下的文件会被 OpenCode 自动加载（可不写 `plugin` 字段），但仍需配置 `provider`。

## 配置

`~/.config/opencode/opencode.jsonc` 最小示例：

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "plugin": [
    ["./plugins/axonhub-discover/index.ts", { "providerID": "axonhub" }]
  ],
  "provider": {
    "axonhub": {
      "name": "海泡菜",
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "baseURL": "https://your-axonhub.example.com/v1"
      },
      // 可选：只写覆盖项。其余由发现填充
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

1. 在 OpenCode 执行 `/connect`，选 **Other**，provider id 填 `axonhub`，粘贴 AxonHub API Key。  
   或在 `options` 里写 `"apiKey": "ah-..."`（不建议写进可分享配置）。
2. 将 `options.baseURL` 设为你的 AxonHub OpenAI 兼容端点（`.../v1`）。
3. **重启 OpenCode**（插件仅在启动时加载）。
4. 打开 `/models`，应能看到自动发现的模型。

## 插件参数

| 参数                | 默认值        | 说明                                                            |
| ------------------- | ------------- | --------------------------------------------------------------- |
| `providerID`        | `axonhub`     | 对应配置里 `provider` 的 key                                    |
| `authID`            | 同 providerID | `auth.json` 中的 key（可与 provider 不同）                      |
| `timeoutMs`         | `15000`       | 请求 `/models` 的超时（毫秒）                                   |
| `reasoningVariants` | 内置一组      | 给可推理模型补的默认 variants；传 `false` 关闭                  |

默认情况下，API 返回中 `capabilities.reasoning: true` 的模型会自动获得思维强度 variants。如果某个模型支持推理但没有档位，应由服务端在 `GET /v1/models` 中上报 `capabilities.reasoning`。

默认 variants 组：

```jsonc
{
  "low": { "reasoningEffort": "low" },
  "medium": { "reasoningEffort": "medium" },
  "high": { "reasoningEffort": "high" },
  "xhigh": { "reasoningEffort": "xhigh" },
  "max": { "reasoningEffort": "max" }
}
```

可以替换成自己的，或关闭：

```jsonc
["./plugins/axonhub-discover/index.ts", {
  "reasoningVariants": { "low": { "reasoningEffort": "low" }, "high": { "reasoningEffort": "high" } }
}]
// 或
["./plugins/axonhub-discover/index.ts", { "reasoningVariants": false }]
```

自定义 provider id 示例：

```jsonc
"plugin": [
  ["./plugins/axonhub-discover/index.ts", {
    "providerID": "my-gateway",
    "authID": "my-gateway",
    "timeoutMs": 20000
  }]
]
```

## 合并规则

- **发现写入**：`name`、`limit`、`modalities`、`reasoning`、`tool_call`、`attachment`、`cost`
- **配置优先**（例如你的 `variants`、自定义 `name`）
- 仅存在于配置、API 未返回的模型会 **保留**
- 发现失败（网络 / 401 / 超时）时，**不改动**现有配置

## 字段映射

AxonHub `GET /v1/models` → OpenCode model：

| API                       | OpenCode           |
| ------------------------- | ------------------ |
| `id`                      | 模型 key           |
| `name`                    | `name`             |
| `context_length`          | `limit.context`    |
| `max_output_tokens`       | `limit.output`     |
| `modalities.input/output` | `modalities`       |
| `capabilities.reasoning`  | `reasoning`        |
| `capabilities.tool_call`  | `tool_call`        |
| `capabilities.vision`     | `attachment: true` |
| `pricing.*`               | `cost.*`           |

## 排障

- **没有新模型**：重启 OpenCode；确认 `providerID` 与配置一致；用 curl 测 key：  
  `curl -H "Authorization: Bearer $KEY" "$BASE_URL/models"`
- **401**：key 缺失或 `authID` 不对，重新 `/connect`
- **路径错误**：`baseURL` 应为 `.../v1`，不要写成 `.../v1/models`
- **variants 没了**：在配置的 `provider.<id>.models.<modelId>.variants` 里保留覆盖

## 许可证

与 AxonHub 仓库相同。
