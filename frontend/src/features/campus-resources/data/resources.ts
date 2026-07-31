import { z } from 'zod';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useSelectedProjectId } from '@/stores/projectStore';
import { apiRequest } from '@/lib/api-client';

export const campusModelDetailSchema = z.object({
  id: z.string(),
  source: z.string(),
  vision: z.boolean(),
  toolCall: z.boolean(),
  reasoning: z.boolean(),
  contextLength: z.number().int().nonnegative(),
  maxOutputTokens: z.number().int().nonnegative().optional(),
  overridden: z.boolean().optional().default(false),
  variesByAPIKey: z.boolean().optional().default(false),
});

const campusResourceApiKeySchema = z.object({
  name: z.string(),
  models: z.array(z.string()),
  modelDetails: z.array(campusModelDetailSchema).optional().default([]),
});

const campusChannelHealthSchema = z.object({
  state: z.enum(['healthy', 'degraded', 'unhealthy', 'recovering', 'unknown']).optional().default('unknown'),
  recentSuccessRate: z.number().nonnegative().optional(),
  recentRequestCount: z.number().int().nonnegative().optional(),
  lastCheckedAt: z.string().optional(),
  lastSuccessAt: z.string().optional(),
  lastFailureCategory: z.string().optional(),
});

const campusResourceChannelSchema = z.object({
  id: z.string().optional(),
  name: z.string(),
  provider: z.string(),
  source: z.enum(['project', 'donated']),
  description: z.string().optional(),
  contributor: z.string(),
  status: z.enum(['enabled', 'disabled']),
  expiresAt: z.string().optional(),
  models: z.array(z.string()).default([]),
  modelCount: z.number().int().nonnegative(),
  effectiveTokens: z.number().int().nonnegative().default(0),
  canProbe: z.boolean().optional().default(false),
  health: campusChannelHealthSchema.optional(),
});

const campusQuotaPeriodSchema = z.object({
  limit: z.number().int().nonnegative().optional(),
  used: z.number().int().nonnegative().optional(),
  remaining: z.number().int().nonnegative().optional(),
  resetAt: z.string().optional(),
});

const campusWalletOverviewSchema = z.object({
  balance: z.number().int().nonnegative().optional(),
  lifetimeEarned: z.number().int().nonnegative().optional(),
  lifetimeSpent: z.number().int().nonnegative().optional(),
});

const campusDonationBenefitSchema = z.object({
  channelId: z.string().optional(),
  name: z.string(),
  expiresAt: z.string().optional(),
  effectiveTokens: z.number().int().nonnegative().optional(),
  rewardEligibleTokens: z.number().int().nonnegative().optional(),
  creditTokens: z.number().int().nonnegative().optional(),
});

const campusUsageOverviewSchema = z.object({
  accountingMethod: z.string().optional(),
  daily: campusQuotaPeriodSchema.optional(),
  weekly: campusQuotaPeriodSchema.optional(),
  wallet: campusWalletOverviewSchema.optional(),
  tokens: z
    .object({
      input: z.number().int().nonnegative().optional(),
      cacheRead: z.number().int().nonnegative().optional(),
      output: z.number().int().nonnegative().optional(),
      effective: z.number().int().nonnegative().optional(),
    })
    .optional(),
  donations: z.array(campusDonationBenefitSchema).optional().default([]),
});

const campusDonationBenefitsSchema = z.preprocess(
  (value) => (Array.isArray(value) ? { channels: value } : value),
  z.object({
    channels: z.array(campusDonationBenefitSchema).optional().default([]),
    totalEffectiveTokens: z.number().int().nonnegative().optional(),
    totalCreditTokens: z.number().int().nonnegative().optional(),
  })
);

const campusChannelProbeAttemptSchema = z.object({
  success: z.boolean(),
  modelID: z.string().min(1),
  statusCode: z.number().int().nonnegative().nullable().optional(),
  error: z.string().optional(),
  latency: z.number().nonnegative(),
});

const campusChannelProbeResultSchema = z.object({
  success: z.boolean(),
  channelID: z.string().min(1),
  modelID: z.string().min(1),
  statusCode: z.number().int().nonnegative().nullable().optional(),
  error: z.string().optional(),
  latency: z.number().nonnegative(),
  attempts: z.array(campusChannelProbeAttemptSchema).min(1),
  health: campusChannelHealthSchema.optional(),
  errorCategory: z.string().optional(),
});

export const campusResourcesSchema = z.object({
  models: z.array(z.string()),
  modelDetails: z.array(campusModelDetailSchema).optional().default([]),
  apiKeys: z.array(campusResourceApiKeySchema),
  channels: z.array(campusResourceChannelSchema),
  usageOverview: campusUsageOverviewSchema.optional(),
  donationBenefits: campusDonationBenefitsSchema.optional(),
});

const campusManagedChannelSchema = z.object({
  id: z.string(),
  name: z.string(),
  models: z.array(campusModelDetailSchema),
});

const campusChannelModelCapabilitiesSchema = z.object({
  channels: z.array(campusManagedChannelSchema),
});

export type CampusModelDetail = z.infer<typeof campusModelDetailSchema>;
export type CampusResourceChannel = z.infer<typeof campusResourceChannelSchema>;
export type CampusManagedChannel = z.infer<typeof campusManagedChannelSchema>;
export type CampusUsageOverview = z.infer<typeof campusUsageOverviewSchema>;
export type CampusDonationBenefits = z.infer<typeof campusDonationBenefitsSchema>;
export type CampusChannelProbeAttempt = z.infer<typeof campusChannelProbeAttemptSchema>;
export type CampusChannelProbeResult = z.infer<typeof campusChannelProbeResultSchema>;

export interface ProbeCampusChannelInput {
  channelID: string;
  modelID?: string;
}

export interface CampusModelCapabilityOverride {
  vision: boolean;
  toolCall: boolean;
  reasoning: boolean;
  contextLength: number;
  maxOutputTokens?: number;
}

export interface UpdateCampusModelCapabilityInput {
  channelID: string;
  modelID: string;
  override: CampusModelCapabilityOverride | null;
}

export function useCampusResources() {
  const selectedProjectId = useSelectedProjectId();

  return useQuery({
    queryKey: ['campusResources', selectedProjectId],
    queryFn: async () => {
      if (!selectedProjectId) {
        throw new Error('A project must be selected before loading resources.');
      }

      const data = await apiRequest<unknown>('/admin/campus/resources', {
        requireAuth: true,
        headers: { 'X-Project-ID': selectedProjectId },
      });

      return campusResourcesSchema.parse(data);
    },
    enabled: !!selectedProjectId,
    refetchInterval: 60_000,
    placeholderData: (previousData) => previousData,
  });
}

export function useProbeCampusChannel() {
  const selectedProjectId = useSelectedProjectId();
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async ({ channelID, modelID }: ProbeCampusChannelInput) => {
      if (!selectedProjectId) {
        throw new Error('A project must be selected before probing a channel.');
      }

      const modelQuery = modelID ? `?model=${encodeURIComponent(modelID)}` : '';
      const data = await apiRequest<unknown>(`/admin/campus/channels/${encodeURIComponent(channelID)}/probe${modelQuery}`, {
        method: 'POST',
        requireAuth: true,
        headers: { 'X-Project-ID': selectedProjectId },
      });

      return campusChannelProbeResultSchema.parse(data);
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['campusResources', selectedProjectId] });
    },
  });
}

export function useChannelModelCapabilities() {
  const selectedProjectId = useSelectedProjectId();

  return useQuery({
    queryKey: ['campusChannelModelCapabilities', selectedProjectId],
    queryFn: async () => {
      if (!selectedProjectId) {
        throw new Error('A project must be selected before loading channel model capabilities.');
      }

      const data = await apiRequest<unknown>('/admin/campus/channel-model-capabilities', {
        requireAuth: true,
        headers: { 'X-Project-ID': selectedProjectId },
      });

      return campusChannelModelCapabilitiesSchema.parse(data);
    },
    enabled: !!selectedProjectId,
  });
}

export function useUpdateChannelModelCapability() {
  const selectedProjectId = useSelectedProjectId();
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (input: UpdateCampusModelCapabilityInput) => {
      if (!selectedProjectId) {
        throw new Error('A project must be selected before updating channel model capabilities.');
      }

      await apiRequest<unknown>('/admin/campus/channel-model-capabilities', {
        method: 'PATCH',
        requireAuth: true,
        headers: { 'X-Project-ID': selectedProjectId },
        body: input,
      });
    },
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['campusChannelModelCapabilities', selectedProjectId] }),
        queryClient.invalidateQueries({ queryKey: ['campusResources', selectedProjectId] }),
      ]);
    },
  });
}
