import { useState } from 'react';
import { Loader2 } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Header } from '@/components/layout/header';
import { Main } from '@/components/layout/main';
import { Skeleton } from '@/components/ui/skeleton';
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { formatNumber } from '@/utils/format-number';
import { useCampusUsageLeaderboard, type CampusUsageLeaderboardTimeWindow } from './data/usage-stats';

const leaderboardPeriods: readonly CampusUsageLeaderboardTimeWindow[] = ['day', 'week', 'month'];

export function CampusUsageLeaderboardPage() {
  const { t } = useTranslation();
  const [timeWindow, setTimeWindow] = useState<CampusUsageLeaderboardTimeWindow>('day');
  const { data = [], isLoading, isFetching, error } = useCampusUsageLeaderboard(timeWindow);
  const periodLabel = t(`usageStats.leaderboard.period.${timeWindow}`);

  if (isLoading) {
    return (
      <div className='flex-1 space-y-4 p-8 pt-6'>
        <Skeleton className='h-8 w-[240px]' />
        <Skeleton className='h-[400px] w-full' />
      </div>
    );
  }

  if (error) {
    return (
      <div className='flex-1 space-y-4 p-8 pt-6'>
        <div className='text-red-500'>{t('common.loadError')}</div>
      </div>
    );
  }

  return (
    <div className='flex flex-1 flex-col overflow-hidden'>
      <Header fixed>
        <div className='flex flex-1 flex-wrap items-start justify-between gap-3'>
          <div>
            <h2 className='text-xl font-bold tracking-tight'>{t('usageStats.leaderboard.title')}</h2>
            <p className='text-sm text-muted-foreground'>{t('usageStats.leaderboard.description', { period: periodLabel })}</p>
          </div>
          <Tabs value={timeWindow} onValueChange={(value) => setTimeWindow(value as CampusUsageLeaderboardTimeWindow)}>
            <TabsList className='grid w-full grid-cols-3 sm:w-auto'>
              {leaderboardPeriods.map((period) => (
                <TabsTrigger key={period} value={period}>
                  {t(`usageStats.leaderboard.period.${period}`)}
                </TabsTrigger>
              ))}
            </TabsList>
          </Tabs>
        </div>
      </Header>

      <Main fixed className='flex flex-col gap-4'>
        <div className='rounded-xl border bg-muted/30 px-4 py-3 text-sm text-muted-foreground'>
          {t('usageStats.leaderboard.privacyNotice')}
        </div>

        <div className='shadow-soft relative flex-1 overflow-auto overflow-x-hidden rounded-2xl border border-[var(--table-border)]'>
          {data.length === 0 ? (
            <div className='flex h-[200px] items-center justify-center rounded-2xl bg-[var(--table-background)]'>
              <div className='text-sm text-muted-foreground'>{t('usageStats.leaderboard.empty', { period: periodLabel })}</div>
            </div>
          ) : (
            <Table className='border-separate border-spacing-0 rounded-2xl bg-[var(--table-background)]'>
              <TableHeader className='sticky top-0 z-20 bg-[var(--table-header)] shadow-sm'>
                <TableRow className='group/row border-0'>
                  <TableHead className='w-16 border-0 text-center text-xs font-semibold uppercase tracking-wider text-muted-foreground'>
                    {t('usageStats.leaderboard.rank')}
                  </TableHead>
                  <TableHead className='border-0 text-xs font-semibold uppercase tracking-wider text-muted-foreground'>
                    {t('usageStats.leaderboard.alias')}
                  </TableHead>
                  <TableHead className='border-0 text-right text-xs font-semibold uppercase tracking-wider text-muted-foreground'>
                    {t('usageStats.leaderboard.recordedTokens')}
                  </TableHead>
                  <TableHead className='border-0 text-right text-xs font-semibold uppercase tracking-wider text-muted-foreground'>
                    {t('usageStats.leaderboard.meteredRequests')}
                  </TableHead>
                  {timeWindow === 'day' && (
                    <TableHead className='border-0 text-right text-xs font-semibold uppercase tracking-wider text-muted-foreground'>
                      {t('usageStats.leaderboard.limitPercent')}
                    </TableHead>
                  )}
                </TableRow>
              </TableHeader>
              <TableBody className='space-y-1 !bg-[var(--table-background)] p-2'>
                {data.map((entry) => (
                  <TableRow
                    key={`${entry.rank}-${entry.publicAlias}`}
                    className={entry.isMe
                      ? 'rounded-xl border-0 !bg-primary/5'
                      : 'group/row table-row-hover rounded-xl border-0 !bg-[var(--table-background)] transition-all duration-200 ease-in-out'}
                  >
                    <TableCell className='border-0 bg-inherit px-4 py-3 text-center text-xs text-muted-foreground'>
                      {entry.rank}
                    </TableCell>
                    <TableCell className='border-0 bg-inherit px-4 py-3 font-medium'>
                      <div className='flex items-center gap-2'>
                        <div className='min-w-0'>
                          <span className='block truncate'>{entry.displayName}</span>
                          {entry.displayName !== entry.publicAlias && (
                            <span className='block truncate text-xs font-normal text-muted-foreground'>
                              {entry.publicAlias}
                            </span>
                          )}
                        </div>
                        {entry.isMe && (
                          <span className='rounded-full bg-primary/10 px-2 py-0.5 text-xs font-medium text-primary'>
                            {t('usageStats.leaderboard.me')}
                          </span>
                        )}
                      </div>
                    </TableCell>
                    <TableCell className='border-0 bg-inherit px-4 py-3 text-right font-mono text-sm'>
                      {formatNumber(entry.recordedTokens)}
                    </TableCell>
                    <TableCell className='border-0 bg-inherit px-4 py-3 text-right font-mono text-sm'>
                      {formatNumber(entry.meteredRequestCount, { digits: 0 })}
                    </TableCell>
                    {timeWindow === 'day' && (
                      <TableCell className='border-0 bg-inherit px-4 py-3 text-right font-mono text-sm'>
                        {entry.limitPercent.toFixed(1)}%
                      </TableCell>
                    )}
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}

          {isFetching && (
            <div className='absolute inset-0 z-30 flex items-center justify-center rounded-2xl bg-background/50'>
              <Loader2 className='h-6 w-6 animate-spin text-muted-foreground' />
            </div>
          )}
        </div>
      </Main>
    </div>
  );
}
