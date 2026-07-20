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
      requestCount
      totalTokens
      totalCost
    }
  }
`;

const CAMPUS_USAGE_LEADERBOARD_QUERY = `
  query GetCampusUsageLeaderboard {
    campusUsageLeaderboard {
      rank
      publicAlias
      isMe
      recordedTokens
      meteredRequestCount
      limitPercent
    }
  }
`;

export const campusUsageLeaderboardEntrySchema = z.object({
  rank: z.number().int().positive(),
  publicAlias: z.string(),
  isMe: z.boolean(),
  recordedTokens: z.number().nonnegative(),
  meteredRequestCount: z.number().int().nonnegative(),
  limitPercent: z.number().nonnegative(),
});

export type CampusUsageLeaderboardEntry = z.infer<typeof campusUsageLeaderboardEntrySchema>;

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

export function useCampusUsageLeaderboard() {
  const selectedProjectId = useSelectedProjectId();

  return useQuery({
    queryKey: ['campusUsageLeaderboard', selectedProjectId],
    queryFn: async () => {
      const headers = selectedProjectId ? { 'X-Project-ID': selectedProjectId } : undefined;
      const data = await graphqlRequest<{ campusUsageLeaderboard: CampusUsageLeaderboardEntry[] }>(
        CAMPUS_USAGE_LEADERBOARD_QUERY,
        undefined,
        headers
      );
      return data.campusUsageLeaderboard.map((item) => campusUsageLeaderboardEntrySchema.parse(item));
    },
    enabled: !!selectedProjectId,
    refetchInterval: 60000,
    placeholderData: (previousData) => previousData,
  });
}
