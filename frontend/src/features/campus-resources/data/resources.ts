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

const campusResourceChannelSchema = z.object({
  name: z.string(),
  provider: z.string(),
  source: z.enum(['project', 'donated']),
  description: z.string().optional(),
  contributor: z.string(),
  status: z.enum(['enabled', 'disabled']),
  expiresAt: z.string().optional(),
  modelCount: z.number().int().nonnegative(),
});

export const campusResourcesSchema = z.object({
  models: z.array(z.string()),
  modelDetails: z.array(campusModelDetailSchema).optional().default([]),
  apiKeys: z.array(campusResourceApiKeySchema),
  channels: z.array(campusResourceChannelSchema),
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
