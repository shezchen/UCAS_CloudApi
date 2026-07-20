import fs from "node:fs/promises"
import os from "node:os"
import path from "node:path"

type PluginOptions = {
  /** provider id in opencode.json, default: axonhub */
  providerID?: string
  /** auth.json key, default: same as providerID */
  authID?: string
  /** request timeout ms, default: 15000 */
  timeoutMs?: number
  /**
   * Variants applied to discovered models with `capabilities.reasoning: true`
   * that have no variants defined in config. Set to false to disable.
   */
  reasoningVariants?: Record<string, any> | false
}

const DEFAULT_REASONING_VARIANTS: Record<string, any> = {
  low: { reasoningEffort: "low" },
  medium: { reasoningEffort: "medium" },
  high: { reasoningEffort: "high" },
  xhigh: { reasoningEffort: "xhigh" },
  max: { reasoningEffort: "max" },
}

type RemoteModel = {
  id?: string
  name?: string
  context_length?: number
  max_context_length?: number
  max_output_tokens?: number
  modalities?: {
    input?: string[]
    output?: string[]
  }
  capabilities?: {
    vision?: boolean
    tool_call?: boolean
    reasoning?: boolean
  }
  pricing?: {
    input?: number
    output?: number
    cache_read?: number
    cache_write?: number
  }
}

function asRecord(value: unknown): Record<string, any> | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined
  return value as Record<string, any>
}

async function readAuthKey(authID: string): Promise<string | undefined> {
  const authPath = path.join(os.homedir(), ".local", "share", "opencode", "auth.json")
  try {
    const raw = await fs.readFile(authPath, "utf8")
    const auth = JSON.parse(raw) as Record<string, { type?: string; key?: string }>
    const entry = auth[authID]
    if (entry?.type === "api" && typeof entry.key === "string" && entry.key.length > 0) {
      return entry.key
    }
  } catch {
    // ignore missing/unreadable auth
  }
  return undefined
}

function mapModel(remote: RemoteModel): Record<string, any> {
  const context = remote.context_length ?? remote.max_context_length
  const output = remote.max_output_tokens
  const inputMods = remote.modalities?.input
  const outputMods = remote.modalities?.output
  const caps = remote.capabilities
  const pricing = remote.pricing

  const model: Record<string, any> = {
    name: remote.name || remote.id,
  }

  if (typeof context === "number" || typeof output === "number") {
    model.limit = {
      ...(typeof context === "number" ? { context } : {}),
      ...(typeof output === "number" ? { output } : {}),
    }
  }

  if (inputMods || outputMods) {
    model.modalities = {
      ...(inputMods ? { input: inputMods } : {}),
      ...(outputMods ? { output: outputMods } : {}),
    }
  }

  if (caps) {
    if (typeof caps.reasoning === "boolean") model.reasoning = caps.reasoning
    if (typeof caps.tool_call === "boolean") model.tool_call = caps.tool_call
    if (caps.vision === true) model.attachment = true
  }

  if (pricing && (typeof pricing.input === "number" || typeof pricing.output === "number")) {
    model.cost = {
      input: pricing.input ?? 0,
      output: pricing.output ?? 0,
      ...(typeof pricing.cache_read === "number" ? { cache_read: pricing.cache_read } : {}),
      ...(typeof pricing.cache_write === "number" ? { cache_write: pricing.cache_write } : {}),
    }
  }

  return model
}

/**
 * OpenCode plugin: auto-discover models from an AxonHub (or any OpenAI-compatible) endpoint.
 *
 * On startup it calls `GET {baseURL}/models` and merges results into `provider.<id>.models`.
 * Existing config entries (e.g. variants) override discovered fields.
 */
export default async (_input: unknown, options: PluginOptions = {}) => {
  const providerID = options.providerID || "axonhub"
  const authID = options.authID || providerID
  const timeoutMs = options.timeoutMs ?? 15_000
  const reasoningVariants =
    options.reasoningVariants === false ? undefined : (options.reasoningVariants ?? DEFAULT_REASONING_VARIANTS)

  return {
    config: async (cfg: Record<string, any>) => {
      const providers = asRecord(cfg.provider)
      if (!providers) return

      const provider = asRecord(providers[providerID])
      if (!provider) return

      const providerOptions = asRecord(provider.options) ?? {}
      const baseURL =
        typeof providerOptions.baseURL === "string" ? providerOptions.baseURL.replace(/\/+$/, "") : undefined
      if (!baseURL) return

      const apiKey =
        (typeof providerOptions.apiKey === "string" && providerOptions.apiKey) || (await readAuthKey(authID))
      if (!apiKey) return

      const existing = asRecord(provider.models) ?? {}
      const next: Record<string, any> = { ...existing }

      try {
        const response = await fetch(`${baseURL}/models`, {
          headers: {
            Authorization: `Bearer ${apiKey}`,
            Accept: "application/json",
          },
          signal: AbortSignal.timeout(timeoutMs),
        })
        if (!response.ok) return

        const body = (await response.json()) as { data?: RemoteModel[] } | RemoteModel[]
        const list = Array.isArray(body) ? body : body.data
        if (!Array.isArray(list) || list.length === 0) return

        for (const remote of list) {
          if (!remote?.id) continue
          const discovered = mapModel(remote)
          const override = asRecord(existing[remote.id]) ?? {}
          // config overrides win (e.g. variants / custom name)
          const merged: Record<string, any> = { ...discovered, ...override }
          // attach default reasoning variants when config didn't define any
          if (!merged.variants && reasoningVariants && remote.capabilities?.reasoning === true) {
            merged.variants = { ...reasoningVariants }
          }
          next[remote.id] = merged
        }

        provider.models = next
      } catch {
        // keep existing config models on discovery failure
      }
    },
  }
}
