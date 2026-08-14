import { useQuery } from '@tanstack/react-query';
import { z } from 'zod';
import { graphqlRequest } from '@/gql/graphql';
import { useSelectedProjectId } from '@/stores/projectStore';
import { usageStatsByUserSchema, type UsageStatsByUser } from '@/features/dashboard/data/dashboard';

const USAGE_STATS_BY_USER_QUERY = `
  query GetUsageStatsByUser($timeWindow: String) {
    usageStatsByUser(timeWindow: $timeWindow) {
      userId
      userName
      avatar
      requestCount
      totalTokens
      totalCost
    }
  }
`;

const CAMPUS_USAGE_LEADERBOARD_QUERY = `
  query GetCampusUsageLeaderboard($timeWindow: String) {
    campusUsageLeaderboard(timeWindow: $timeWindow) {
      rank
      displayName
      publicAlias
      avatar
      isMe
      recordedTokens
      meteredRequestCount
      limitPercent
    }
  }
`;

const CAMPUS_MODEL_USAGE_LEADERBOARD_QUERY = `
  query GetCampusModelUsageLeaderboard($timeWindow: String) {
    campusModelUsageLeaderboard(timeWindow: $timeWindow) {
      rank
      modelId
      effectiveTokens
      inputTokens
      cachedReadTokens
      outputTokens
      meteredRequestCount
    }
  }
`;

export const campusUsageLeaderboardEntrySchema = z.object({
  rank: z.number().int().positive(),
  displayName: z.string(),
  publicAlias: z.string(),
  avatar: z.string().optional().nullable(),
  isMe: z.boolean(),
  recordedTokens: z.number().nonnegative(),
  meteredRequestCount: z.number().int().nonnegative(),
  limitPercent: z.number().nonnegative(),
});

export const campusModelUsageLeaderboardEntrySchema = z.object({
  rank: z.number().int().positive(),
  modelId: z.string(),
  effectiveTokens: z.number().nonnegative().optional().default(0),
  inputTokens: z.number().nonnegative().optional().default(0),
  cachedReadTokens: z.number().nonnegative().optional().default(0),
  outputTokens: z.number().nonnegative().optional().default(0),
  meteredRequestCount: z.number().int().nonnegative().optional().default(0),
});

export type CampusUsageLeaderboardEntry = z.infer<typeof campusUsageLeaderboardEntrySchema>;
export type CampusModelUsageLeaderboardEntry = z.infer<typeof campusModelUsageLeaderboardEntrySchema>;
export type CampusUsageLeaderboardTimeWindow = 'day' | 'week' | 'month';

export function useUsageStatsByUser(timeWindow?: string) {
  const selectedProjectId = useSelectedProjectId();

  return useQuery({
    queryKey: ['usageStatsByUser', timeWindow, selectedProjectId],
    queryFn: async () => {
      const headers = selectedProjectId ? { 'X-Project-ID': selectedProjectId } : undefined;
      const data = await graphqlRequest<{ usageStatsByUser: UsageStatsByUser[] }>(
        USAGE_STATS_BY_USER_QUERY,
        { timeWindow },
        headers
      );
      return data.usageStatsByUser.map((item) => usageStatsByUserSchema.parse(item));
    },
    enabled: !!selectedProjectId,
    refetchInterval: 60000,
    placeholderData: (previousData) => previousData,
  });
}

export function useCampusUsageLeaderboard(
  timeWindow: CampusUsageLeaderboardTimeWindow = 'day',
  options?: { enabled?: boolean }
) {
  const selectedProjectId = useSelectedProjectId();

  return useQuery({
    queryKey: ['campusUsageLeaderboard', timeWindow, selectedProjectId],
    queryFn: async () => {
      const headers = selectedProjectId ? { 'X-Project-ID': selectedProjectId } : undefined;
      const data = await graphqlRequest<{ campusUsageLeaderboard: CampusUsageLeaderboardEntry[] }>(
        CAMPUS_USAGE_LEADERBOARD_QUERY,
        { timeWindow },
        headers
      );
      return data.campusUsageLeaderboard.map((item) => campusUsageLeaderboardEntrySchema.parse(item));
    },
    enabled: !!selectedProjectId && (options?.enabled ?? true),
    refetchInterval: 60000,
    placeholderData: (previousData) => previousData,
  });
}

export function useCampusModelUsageLeaderboard(
  period: CampusUsageLeaderboardTimeWindow = 'day',
  options?: { enabled?: boolean }
) {
  const selectedProjectId = useSelectedProjectId();

  return useQuery({
    queryKey: ['campusModelUsageLeaderboard', period, selectedProjectId],
    queryFn: async () => {
      const headers = selectedProjectId ? { 'X-Project-ID': selectedProjectId } : undefined;
      const data = await graphqlRequest<{ campusModelUsageLeaderboard?: CampusModelUsageLeaderboardEntry[] }>(
        CAMPUS_MODEL_USAGE_LEADERBOARD_QUERY,
        { timeWindow: period },
        headers
      );
      return (data.campusModelUsageLeaderboard ?? []).map((item) => campusModelUsageLeaderboardEntrySchema.parse(item));
    },
    enabled: !!selectedProjectId && (options?.enabled ?? true),
    refetchInterval: 60000,
    placeholderData: (previousData) => previousData,
  });
}
