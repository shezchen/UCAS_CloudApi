import { z } from 'zod';
import { useQuery } from '@tanstack/react-query';
import { useSelectedProjectId } from '@/stores/projectStore';
import { apiRequest } from '@/lib/api-client';

const campusResourceApiKeySchema = z.object({
  name: z.string(),
  models: z.array(z.string()),
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
  apiKeys: z.array(campusResourceApiKeySchema),
  channels: z.array(campusResourceChannelSchema),
});

export type CampusResourceChannel = z.infer<typeof campusResourceChannelSchema>;

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
