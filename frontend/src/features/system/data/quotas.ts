import { useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';
import { graphqlRequest } from '@/gql/graphql';
import { getTokenFromStorage } from '@/stores/authStore';
import { useSelectedProjectId } from '@/stores/projectStore';

const CHECK_PROVIDER_QUOTAS_QUERY = `
  mutation CheckProviderQuotas {
    checkProviderQuotas
  }
`;

const RESET_CHANNEL_QUOTA_NOW_MUTATION = `
  mutation ResetChannelQuotaNow($channelID: ID!) {
    resetChannelQuotaNow(channelID: $channelID)
  }
`;

export async function checkProviderQuotas() {
  return graphqlRequest(CHECK_PROVIDER_QUOTAS_QUERY);
}

export async function resetChannelQuotaNow(channelID: string) {
  return graphqlRequest(RESET_CHANNEL_QUOTA_NOW_MUTATION, { channelID });
}

type ProviderQuotaDataCommon = {
  plan_type?: string;
  error?: string;
};

type ProviderClaudeQuotaData = ProviderQuotaDataCommon & {
  windows?: {
    '5h'?: { utilization?: number; reset?: number; status?: string };
    '7d'?: { utilization?: number; reset?: number; status?: string };
    overage?: { utilization?: number; reset?: number; status?: string };
  };
  representative_claim?: string;
};

type ProviderCodexQuotaData = ProviderQuotaDataCommon & {
  rate_limit?: {
    primary_window?: {
      used_percent?: number;
      reset_at?: number;
      reset_after_seconds?: number;
      limit_window_seconds?: number;
    };
    secondary_window?: {
      used_percent?: number;
      reset_at?: number;
      reset_after_seconds?: number;
      limit_window_seconds?: number;
    };
  };
};

type CopilotQuotaSnapshot = {
  entitlement: number;
  has_quota: boolean;
  overage_count: number;
  overage_permitted: boolean;
  percent_remaining: number;
  quota_id: string;
  quota_remaining: number;
  quota_reset_at: number;
  remaining: number;
  timestamp_utc: string;
  unlimited: boolean;
};

type ProviderGitHubCopilotQuotaData = ProviderQuotaDataCommon & {
  limited_user_quotas?: {
    chat?: number;
    completions?: number;
    [key: string]: number | undefined;
  };
  quota_snapshots?: {
    chat?: CopilotQuotaSnapshot;
    completions?: CopilotQuotaSnapshot;
    premium_interactions?: CopilotQuotaSnapshot;
    premium_models?: CopilotQuotaSnapshot;
    [key: string]: CopilotQuotaSnapshot | undefined;
  };
  total_quotas?: {
    chat?: number;
    completions?: number;
    [key: string]: number | undefined;
  };
};

export type NanoGPTQuotaWindow = {
  used?: number;
  remaining?: number;
  percentUsed?: number;
  resetAt?: number;
};

export type ProviderNanoGPTQuotaData = ProviderQuotaDataCommon & {
  state?: string;
  active?: boolean;
  allowOverage?: boolean;
  limits?: {
    weeklyInputTokens?: number;
    dailyImages?: number;
    dailyInputTokens?: number;
  };
  windows?: {
    weeklyInputTokens?: NanoGPTQuotaWindow | null;
    dailyImages?: NanoGPTQuotaWindow | null;
    dailyInputTokens?: NanoGPTQuotaWindow | null;
  };
  period?: { currentPeriodEnd?: string };
};

export type ProviderWaferQuotaData = ProviderQuotaDataCommon & {
  current_period_used_percent?: number | null;
  remaining_included_requests?: number | null;
  included_request_limit?: number | null;
  overage_request_count?: number | null;
  window_start?: string | null;
  window_end?: string | null;
  plan_tier?: string | null;
};

export type ProviderSyntheticQuotaData = ProviderQuotaDataCommon & {
  weeklyTokenLimit?: {
    percentRemaining?: number | null;
    remainingCredits?: string | null;
    maxCredits?: string | null;
    nextRegenAt?: string | null;
  } | null;
  rollingFiveHourLimit?: {
    limited?: boolean | null;
    remaining?: number | null;
    max?: number | null;
    nextTickAt?: string | null;
    tickPercent?: number | null;
  } | null;
};

export type ProviderNeuralWattQuotaData = ProviderQuotaDataCommon & {
  balance?: { credits_remaining_usd?: number | null; total_credits_usd?: number | null } | null;
  subscription?: {
    kwh_included?: number | null;
    kwh_used?: number | null;
    kwh_remaining?: number | null;
    in_overage?: boolean | null;
    status?: string | null;
    plan?: string | null;
    kwh_reset_date?: string | null;
  } | null;
};

export type ProviderApertisQuotaData = ProviderQuotaDataCommon & {
  is_subscriber?: boolean;
  payg?: {
    account_credits?: number;
    token_used?: number;
    token_total?: number | string;
    token_remaining?: number | string;
    token_is_unlimited?: boolean;
    token_monthly_limit_usd?: number;
    token_monthly_used_usd?: number;
    monthly_reset_day?: number;
  };
  subscription?: {
    plan_type?: string;
    status?: string;
    cycle_quota_limit?: number;
    cycle_quota_used?: number;
    cycle_quota_remaining?: number;
    cycle_start?: string;
    cycle_end?: string;
    payg_fallback_enabled?: boolean;
    payg_spent_usd?: number;
    payg_limit_usd?: number;
  };
};

export type OpenCodeGoQuotaWindow = {
  usage_percent?: number;
  reset_in_seconds?: number;
  reset_time?: string;
  status?: string;
  percent_remaining?: number;
};

export type ProviderOpenCodeGoQuotaData = ProviderQuotaDataCommon & {
  windows?: {
    rolling?: OpenCodeGoQuotaWindow;
    weekly?: OpenCodeGoQuotaWindow;
    monthly?: OpenCodeGoQuotaWindow;
  };
};

export type ClineQuotaWindow = {
  items_count: number;
  used_cost_units: number;
  limit_cost_units: number;
  remaining_cost_units: number;
  credits_used: number;
  usage_ratio?: number;
  usage_percent?: number;
  next_reset_at?: string | null;
};

type ClineBalance = {
  raw_balance?: number | null;
  unit_note?: string;
};

type ClineUsageFetch = {
  pages: number;
  items_seen: number;
  truncated: boolean;
};

type ProviderClinePassQuotaData = ProviderQuotaDataCommon & {
  model_scope: 'cline_pass_only' | 'mixed' | 'unknown';
  status_basis: string;
  pool: 'cline_pass';
  pool_note?: string;
  cost_scale: number;
  balance: ClineBalance;
  windows: {
    last5h: ClineQuotaWindow;
    last7d: ClineQuotaWindow;
    last30d: ClineQuotaWindow;
  };
  usage_fetch: ClineUsageFetch;
};

type ProviderClineDirectQuotaData = ProviderQuotaDataCommon & {
  model_scope: 'direct_only';
  status_basis: string;
  pool: 'direct_credit' | string;
  pool_note?: string;
  balance: ClineBalance;
  cost_scale?: never;
  windows?: never;
  usage_fetch?: never;
};

type ProviderClineErrorQuotaData = ProviderQuotaDataCommon & {
  model_scope?: undefined;
  status_basis?: string;
  pool?: string;
  balance?: ClineBalance;
  cost_scale?: never;
  windows?: never;
  usage_fetch?: never;
};

export type ProviderClineQuotaData = ProviderClinePassQuotaData | ProviderClineDirectQuotaData | ProviderClineErrorQuotaData;

export function isClinePassPoolQuotaData(qd: ProviderClineQuotaData): qd is ProviderClinePassQuotaData {
  return qd.pool === 'cline_pass';
}

export type ProviderQuotaChannel = {
  id: string;
  name: string;
  quotaStatus: {
    status: 'available' | 'warning' | 'exhausted' | 'unknown';
    nextResetAt: string | null;
    ready: boolean;
  };
} & (
  | {
      type: 'claudecode';
      quotaStatus: {
        quotaData: ProviderClaudeQuotaData;
      };
    }
  | {
      type: 'codex';
      quotaStatus: {
        quotaData: ProviderCodexQuotaData;
      };
    }
  | {
      type: 'cline';
      quotaStatus: {
        quotaData: ProviderClineQuotaData;
      };
    }
  | {
      type: 'github_copilot';
      quotaStatus: {
        quotaData: ProviderGitHubCopilotQuotaData;
      };
    }
  | {
      type: 'nanogpt';
      quotaStatus: {
        quotaData: ProviderNanoGPTQuotaData;
      };
    }
  | {
      type: 'nanogpt_responses';
      quotaStatus: {
        quotaData: ProviderNanoGPTQuotaData;
      };
    }
  | {
      type: 'opencode_go' | 'opencode_go_anthropic';
      workspaceId?: string | null;
      quotaStatus: {
        quotaData: ProviderOpenCodeGoQuotaData;
      };
    }
  | {
      type: 'openai' | 'openai_responses';
      providerType: 'wafer';
      quotaStatus: {
        quotaData: ProviderWaferQuotaData;
      };
    }
  | {
      type: 'openai' | 'openai_responses';
      providerType: 'synthetic';
      quotaStatus: {
        quotaData: ProviderSyntheticQuotaData;
      };
    }
  | {
      type: 'openai' | 'openai_responses';
      providerType: 'neuralwatt';
      quotaStatus: {
        quotaData: ProviderNeuralWattQuotaData;
      };
    }
  | {
      type: 'openai' | 'openai_responses';
      providerType: 'apertis';
      quotaStatus: {
        quotaData: ProviderApertisQuotaData;
      };
    }
  | {
      type: 'openai' | 'openai_responses';
      providerType?: undefined;
      quotaStatus: {
        quotaData: ProviderQuotaDataCommon;
      };
    }
);

function finiteNumber(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

function maxDefined(values: Array<number | undefined>): number | undefined {
  const defined = values.filter((value): value is number => value !== undefined);
  return defined.length > 0 ? Math.max(...defined) : undefined;
}

function clampPercentage(value: number | undefined): number | undefined {
  return value === undefined ? undefined : Math.min(100, Math.max(0, value));
}

/**
 * Returns the most constrained provider-reported quota window as a used
 * percentage. `undefined` means the provider did not report a measurable
 * quota; callers must not present that as either 0% used or 100% remaining.
 */
export function getProviderQuotaUsagePercentage(channel: ProviderQuotaChannel): number | undefined {
  if (channel.type === 'claudecode') {
    const windows = channel.quotaStatus.quotaData.windows;
    const usage = maxDefined([
      finiteNumber(windows?.['5h']?.utilization),
      finiteNumber(windows?.['7d']?.utilization),
      finiteNumber(windows?.overage?.utilization),
    ]);
    return clampPercentage(usage === undefined ? undefined : usage * 100);
  }

  if (channel.type === 'codex') {
    const rateLimit = channel.quotaStatus.quotaData.rate_limit;
    return clampPercentage(
      maxDefined([finiteNumber(rateLimit?.primary_window?.used_percent), finiteNumber(rateLimit?.secondary_window?.used_percent)])
    );
  }

  if (channel.type === 'cline') {
    const data = channel.quotaStatus.quotaData;
    if (!isClinePassPoolQuotaData(data)) return undefined;
    return clampPercentage(
      maxDefined(
        [data.windows.last5h, data.windows.last7d, data.windows.last30d].map((window) => {
          const explicit = finiteNumber(window?.usage_percent);
          if (explicit !== undefined) return explicit;
          const ratio = finiteNumber(window?.usage_ratio);
          return ratio === undefined ? undefined : ratio * 100;
        })
      )
    );
  }

  if (channel.type === 'github_copilot') {
    const data = channel.quotaStatus.quotaData;
    const remainingPercentages: number[] = [];

    if (data.limited_user_quotas) {
      for (const [key, remaining] of Object.entries(data.limited_user_quotas)) {
        const remainingValue = finiteNumber(remaining);
        const total = finiteNumber(data.total_quotas?.[key]);
        if (remainingValue !== undefined && total !== undefined && total > 0) {
          remainingPercentages.push((remainingValue / total) * 100);
        }
      }
    }

    if (data.quota_snapshots) {
      for (const snapshot of Object.values(data.quota_snapshots)) {
        const remaining = snapshot?.unlimited ? undefined : finiteNumber(snapshot?.percent_remaining);
        if (remaining !== undefined) remainingPercentages.push(remaining);
      }
    }

    return remainingPercentages.length === 0 ? undefined : clampPercentage(100 - Math.min(...remainingPercentages));
  }

  if (channel.type === 'nanogpt' || channel.type === 'nanogpt_responses') {
    const windows = channel.quotaStatus.quotaData.windows;
    const usage = maxDefined([
      finiteNumber(windows?.weeklyInputTokens?.percentUsed),
      finiteNumber(windows?.dailyInputTokens?.percentUsed),
      finiteNumber(windows?.dailyImages?.percentUsed),
    ]);
    return clampPercentage(usage === undefined ? undefined : usage * 100);
  }

  if (channel.type === 'opencode_go' || channel.type === 'opencode_go_anthropic') {
    const windows = channel.quotaStatus.quotaData.windows;
    return clampPercentage(
      maxDefined([
        finiteNumber(windows?.rolling?.usage_percent),
        finiteNumber(windows?.weekly?.usage_percent),
        finiteNumber(windows?.monthly?.usage_percent),
      ])
    );
  }

  if ((channel.type === 'openai' || channel.type === 'openai_responses') && channel.providerType === 'wafer') {
    return clampPercentage(finiteNumber(channel.quotaStatus.quotaData.current_period_used_percent));
  }

  if ((channel.type === 'openai' || channel.type === 'openai_responses') && channel.providerType === 'synthetic') {
    const remaining = finiteNumber(channel.quotaStatus.quotaData.weeklyTokenLimit?.percentRemaining);
    return clampPercentage(remaining === undefined ? undefined : 100 - remaining);
  }

  if ((channel.type === 'openai' || channel.type === 'openai_responses') && channel.providerType === 'neuralwatt') {
    const included = finiteNumber(channel.quotaStatus.quotaData.subscription?.kwh_included);
    const used = finiteNumber(channel.quotaStatus.quotaData.subscription?.kwh_used);
    return clampPercentage(included !== undefined && included > 0 && used !== undefined ? (used / included) * 100 : undefined);
  }

  if ((channel.type === 'openai' || channel.type === 'openai_responses') && channel.providerType === 'apertis') {
    const data = channel.quotaStatus.quotaData;
    if (data.is_subscriber) {
      const limit = finiteNumber(data.subscription?.cycle_quota_limit);
      const used = finiteNumber(data.subscription?.cycle_quota_used);
      if (limit !== undefined && limit > 0 && used !== undefined) {
        return clampPercentage((used / limit) * 100);
      }
    }
    if (!data.payg?.token_is_unlimited) {
      const total = finiteNumber(data.payg?.token_total);
      const used = finiteNumber(data.payg?.token_used);
      if (total !== undefined && total > 0 && used !== undefined) {
        return clampPercentage((used / total) * 100);
      }
    }
  }

  return undefined;
}

type ProviderQuotaStatusNode = {
  status: 'available' | 'warning' | 'exhausted' | 'unknown';
  nextResetAt: string | null;
  ready: boolean;
  quotaData: unknown;
  providerType: string;
};

type QueryChannelNode = {
  id: string;
  name: string;
  type: string;
  providerQuotaStatus: ProviderQuotaStatusNode | null;
};

type ProviderQuotaViewResponse = {
  channels: QueryChannelNode[];
};

type QueryChannelNodeWithQuota = QueryChannelNode & {
  providerQuotaStatus: ProviderQuotaStatusNode;
};

function hasProviderQuotaStatus(node: QueryChannelNode | null | undefined): node is QueryChannelNodeWithQuota {
  return node?.providerQuotaStatus != null;
}

function parseChannelNode(node: QueryChannelNodeWithQuota): ProviderQuotaChannel {
  const quotaStatus = node.providerQuotaStatus;
  const providerType = quotaStatus.providerType;

  const base = {
    id: node.id,
    name: node.name,
    quotaStatus: {
      status: quotaStatus.status,
      nextResetAt: quotaStatus.nextResetAt,
      ready: quotaStatus.ready,
    },
  };

  if (node.type === 'claudecode') {
    return {
      ...base,
      type: 'claudecode' as const,
      quotaStatus: { ...base.quotaStatus, quotaData: node.providerQuotaStatus.quotaData as ProviderClaudeQuotaData },
    };
  }
  if (node.type === 'codex') {
    return {
      ...base,
      type: 'codex' as const,
      quotaStatus: { ...base.quotaStatus, quotaData: node.providerQuotaStatus.quotaData as ProviderCodexQuotaData },
    };
  }
  if (node.type === 'cline') {
    return {
      ...base,
      type: 'cline' as const,
      quotaStatus: { ...base.quotaStatus, quotaData: node.providerQuotaStatus.quotaData as ProviderClineQuotaData },
    };
  }
  if (node.type === 'github_copilot') {
    return {
      ...base,
      type: 'github_copilot' as const,
      quotaStatus: { ...base.quotaStatus, quotaData: node.providerQuotaStatus.quotaData as ProviderGitHubCopilotQuotaData },
    };
  }
  if (node.type === 'nanogpt') {
    return {
      ...base,
      type: 'nanogpt' as const,
      quotaStatus: { ...base.quotaStatus, quotaData: node.providerQuotaStatus.quotaData as ProviderNanoGPTQuotaData },
    };
  }
  if (node.type === 'nanogpt_responses') {
    return {
      ...base,
      type: 'nanogpt_responses' as const,
      quotaStatus: { ...base.quotaStatus, quotaData: node.providerQuotaStatus.quotaData as ProviderNanoGPTQuotaData },
    };
  }
  if (node.type === 'opencode_go' || node.type === 'opencode_go_anthropic') {
    return {
      ...base,
      type: node.type as 'opencode_go' | 'opencode_go_anthropic',
      workspaceId: null,
      quotaStatus: { ...base.quotaStatus, quotaData: node.providerQuotaStatus.quotaData as ProviderOpenCodeGoQuotaData },
    };
  }
  if (node.type === 'openai' || node.type === 'openai_responses') {
    const typeVal = node.type as 'openai' | 'openai_responses';
    if (providerType === 'wafer') {
      return {
        ...base,
        type: typeVal,
        providerType: 'wafer' as const,
        quotaStatus: { ...base.quotaStatus, quotaData: node.providerQuotaStatus.quotaData as ProviderWaferQuotaData },
      };
    }
    if (providerType === 'synthetic') {
      return {
        ...base,
        type: typeVal,
        providerType: 'synthetic' as const,
        quotaStatus: { ...base.quotaStatus, quotaData: node.providerQuotaStatus.quotaData as ProviderSyntheticQuotaData },
      };
    }
    if (providerType === 'neuralwatt') {
      return {
        ...base,
        type: typeVal,
        providerType: 'neuralwatt' as const,
        quotaStatus: { ...base.quotaStatus, quotaData: node.providerQuotaStatus.quotaData as ProviderNeuralWattQuotaData },
      };
    }
    if (providerType === 'apertis') {
      return {
        ...base,
        type: typeVal,
        providerType: 'apertis' as const,
        quotaStatus: { ...base.quotaStatus, quotaData: node.providerQuotaStatus.quotaData as ProviderApertisQuotaData },
      };
    }
    return {
      ...base,
      type: typeVal,
      providerType: undefined,
      quotaStatus: { ...base.quotaStatus, quotaData: node.providerQuotaStatus.quotaData as ProviderQuotaDataCommon },
    };
  }

  return {
    ...base,
    type: node.type as ProviderQuotaChannel['type'],
    quotaStatus: { ...base.quotaStatus, quotaData: node.providerQuotaStatus.quotaData as ProviderQuotaDataCommon },
  };
}

export function useProviderQuotaStatuses() {
  const selectedProjectId = useSelectedProjectId();
  const query = useQuery({
    queryKey: ['provider-quotas', selectedProjectId],
    queryFn: async () => {
      const token = getTokenFromStorage();
      const response = await fetch('/admin/provider-quotas', {
        headers: {
          ...(token ? { Authorization: `Bearer ${token}` } : {}),
          ...(selectedProjectId ? { 'X-Project-ID': selectedProjectId } : {}),
        },
      });
      if (!response.ok) {
        throw new Error(`Failed to load provider quotas (${response.status})`);
      }
      return (await response.json()) as ProviderQuotaViewResponse;
    },
    refetchInterval: 60000,
    enabled: !!selectedProjectId,
  });

  const channels = useMemo(
    () => (query.data?.channels ?? []).filter(hasProviderQuotaStatus).map(parseChannelNode),
    [query.data]
  );

  return {
    channels,
    isLoading: query.isLoading,
    isError: query.isError,
    error: query.error,
    isFetching: query.isFetching,
  };
}
