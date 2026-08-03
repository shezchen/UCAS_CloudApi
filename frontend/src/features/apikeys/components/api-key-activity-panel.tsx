import { useMemo, useState } from 'react';
import { Activity, CircleCheck, CircleX, Clock3, Loader2 } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { formatNumber } from '@/utils/format-number';
import { Badge } from '@/components/ui/badge';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { useCampusApiActivity } from '../data/apikeys';

const ALL_API_KEYS = 'all';

type ActivityResultKind = 'success' | 'error' | 'pending';

function activityResultKind(status: string, statusCode?: number): ActivityResultKind {
  const normalizedStatus = status.toLocaleLowerCase();
  if (['error', 'failed', 'canceled', 'cancelled'].includes(normalizedStatus) || (statusCode !== undefined && statusCode >= 400)) {
    return 'error';
  }
  if (normalizedStatus === 'completed') {
    return 'success';
  }
  return 'pending';
}

function formatEventTime(value: string, locale: string) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;

  return new Intl.DateTimeFormat(locale, {
    month: 'numeric',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  }).format(date);
}

export function ApiKeyActivityPanel() {
  const { t, i18n } = useTranslation();
  const locale = i18n.language.startsWith('zh') ? 'zh-CN' : 'en-US';
  const [selectedApiKeyId, setSelectedApiKeyId] = useState(ALL_API_KEYS);
  const allActivity = useCampusApiActivity();
  const filteredActivity = useCampusApiActivity(selectedApiKeyId === ALL_API_KEYS ? undefined : selectedApiKeyId, {
    enabled: selectedApiKeyId !== ALL_API_KEYS,
  });
  const activeQuery = selectedApiKeyId === ALL_API_KEYS ? allActivity : filteredActivity;
  const activity = activeQuery.data;

  const totals = useMemo(
    () =>
      (activity?.apiKeys ?? []).reduce(
        (result, apiKey) => ({
          inputTokens: result.inputTokens + apiKey.inputTokens,
          cachedReadTokens: result.cachedReadTokens + apiKey.cachedReadTokens,
          outputTokens: result.outputTokens + apiKey.outputTokens,
          effectiveTokens: result.effectiveTokens + apiKey.effectiveTokens,
          successCount: result.successCount + apiKey.successCount,
          errorCount: result.errorCount + apiKey.errorCount,
        }),
        {
          inputTokens: 0,
          cachedReadTokens: 0,
          outputTokens: 0,
          effectiveTokens: 0,
          successCount: 0,
          errorCount: 0,
        }
      ),
    [activity?.apiKeys]
  );

  return (
    <Card className='mb-5 shrink-0 gap-3 py-4 shadow-none' data-testid='api-key-activity-panel'>
      <CardHeader className='gap-3 px-4 sm:px-5'>
        <div className='flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between'>
          <div className='min-w-0'>
            <CardTitle className='flex items-center gap-2 text-base'>
              <Activity className='text-primary size-4' aria-hidden='true' />
              {t('apikeys.activity.title')}
            </CardTitle>
            <CardDescription className='mt-1'>{t('apikeys.activity.description')}</CardDescription>
          </div>
          <Select value={selectedApiKeyId} onValueChange={setSelectedApiKeyId}>
            <SelectTrigger className='w-full sm:w-60' data-testid='api-key-activity-filter'>
              <SelectValue />
            </SelectTrigger>
            <SelectContent align='end'>
              <SelectItem value={ALL_API_KEYS}>{t('apikeys.activity.allKeys')}</SelectItem>
              {(allActivity.data?.apiKeys ?? []).map((apiKey) => (
                <SelectItem key={apiKey.id} value={apiKey.id}>
                  {apiKey.name}
                  {apiKey.suffix ? ` · …${apiKey.suffix}` : ''}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      </CardHeader>

      <CardContent className='space-y-3 px-4 sm:px-5'>
        {activeQuery.isLoading ? (
          <div className='text-muted-foreground flex min-h-36 items-center justify-center gap-2 text-sm' role='status'>
            <Loader2 className='size-4 animate-spin' aria-hidden='true' />
            {t('apikeys.activity.loading')}
          </div>
        ) : activeQuery.error ? (
          <div className='text-muted-foreground flex min-h-24 items-center justify-center rounded-lg border border-dashed px-4 text-center text-sm'>
            {t('apikeys.activity.unavailable')}
          </div>
        ) : (
          <>
            <div className='grid gap-2 sm:grid-cols-2 lg:grid-cols-4' data-testid='api-key-activity-summary'>
              <div className='bg-muted/20 rounded-lg border px-3 py-2'>
                <div className='text-lg font-semibold tabular-nums'>{formatNumber(totals.effectiveTokens)}</div>
                <div className='text-muted-foreground text-xs'>{t('apikeys.activity.effectiveTokens')}</div>
              </div>
              <div className='bg-muted/20 rounded-lg border px-3 py-2'>
                <div className='text-lg font-semibold tabular-nums'>{formatNumber(totals.inputTokens)}</div>
                <div className='text-muted-foreground text-xs'>{t('apikeys.columns.inputTokens')}</div>
              </div>
              <div className='bg-muted/20 rounded-lg border px-3 py-2'>
                <div className='text-lg font-semibold tabular-nums'>{formatNumber(totals.cachedReadTokens)}</div>
                <div className='text-muted-foreground text-xs'>{t('apikeys.activity.cachedReadTokens')}</div>
              </div>
              <div className='bg-muted/20 rounded-lg border px-3 py-2'>
                <div className='text-lg font-semibold tabular-nums'>{formatNumber(totals.outputTokens)}</div>
                <div className='text-muted-foreground text-xs'>{t('apikeys.columns.outputTokens')}</div>
              </div>
            </div>

            <div className='flex flex-wrap items-center gap-2 text-xs'>
              <Badge variant='outline' className='gap-1 border-emerald-500/30 text-emerald-700 dark:text-emerald-400'>
                <CircleCheck className='size-3' aria-hidden='true' />
                {t('apikeys.activity.successCount', { count: totals.successCount })}
              </Badge>
              <Badge variant='outline' className='gap-1 border-red-500/30 text-red-700 dark:text-red-400'>
                <CircleX className='size-3' aria-hidden='true' />
                {t('apikeys.activity.errorCount', { count: totals.errorCount })}
              </Badge>
              <span className='text-muted-foreground ml-auto flex items-center gap-1'>
                <Clock3 className='size-3' aria-hidden='true' />
                {t('apikeys.activity.retention')}
              </span>
            </div>

            {(activity?.events.length ?? 0) === 0 ? (
              <div className='text-muted-foreground flex min-h-20 items-center justify-center rounded-lg border border-dashed px-4 text-sm'>
                {t('apikeys.activity.empty')}
              </div>
            ) : (
              <div className='max-h-56 overflow-auto rounded-lg border' data-testid='api-key-activity-events'>
                <Table>
                  <TableHeader className='bg-background sticky top-0 z-10'>
                    <TableRow>
                      <TableHead>{t('apikeys.activity.time')}</TableHead>
                      <TableHead>{t('apikeys.activity.apiKey')}</TableHead>
                      <TableHead>{t('apikeys.activity.model')}</TableHead>
                      <TableHead>{t('apikeys.activity.result')}</TableHead>
                      <TableHead>{t('apikeys.activity.detail')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {(activity?.events ?? []).map((event) => {
                      const resultKind = activityResultKind(event.status, event.statusCode);
                      const isError = resultKind === 'error';
                      const isPending = resultKind === 'pending';
                      const errorCategory =
                        event.errorCategory === 'upstream_quota' ? t('apikeys.activity.upstreamQuota') : event.errorCategory;
                      const detail = isPending
                        ? t('apikeys.activity.inProgressDetail', { status: event.status || t('apikeys.activity.unknownStatus') })
                        : isError
                          ? [errorCategory, event.errorMessage].filter(Boolean).join(' · ')
                          : event.latencyMs === undefined
                            ? t('apikeys.activity.success')
                            : t('apikeys.activity.latency', { value: Math.round(event.latencyMs) });
                      const hasAttemptChain = event.attempts.length > 0;

                      return (
                        <TableRow key={event.requestId} data-testid='api-key-activity-event-row'>
                          <TableCell className='text-xs whitespace-nowrap'>
                            <time dateTime={event.createdAt}>{formatEventTime(event.createdAt, locale)}</time>
                          </TableCell>
                          <TableCell className='max-w-48'>
                            <span className='block truncate text-sm font-medium' title={event.apiKeyName}>
                              {event.apiKeyName}
                            </span>
                            {event.apiKeySuffix && (
                              <span className='text-muted-foreground block font-mono text-xs'>…{event.apiKeySuffix}</span>
                            )}
                          </TableCell>
                          <TableCell className='max-w-56 font-mono text-xs'>
                            <span className='block truncate' title={event.model}>
                              {event.model || '—'}
                            </span>
                          </TableCell>
                          <TableCell>
                            <Badge
                              variant='outline'
                              className={
                                isError
                                  ? 'border-red-500/30 text-red-700 dark:text-red-400'
                                  : isPending
                                    ? 'border-amber-500/30 text-amber-700 dark:text-amber-400'
                                    : 'border-emerald-500/30 text-emerald-700 dark:text-emerald-400'
                              }
                            >
                              {isPending
                                ? t('apikeys.activity.inProgress')
                                : (event.statusCode ?? t(isError ? 'apikeys.activity.error' : 'apikeys.activity.success'))}
                            </Badge>
                          </TableCell>
                          <TableCell className='max-w-96 text-xs'>
                            <div className='space-y-1.5'>
                              <span className={isError ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'} title={detail}>
                                {detail || '—'}
                              </span>
                              {event.recovered && (
                                <Badge variant='outline' className='ml-2 border-blue-500/30 text-blue-700 dark:text-blue-400'>
                                  {t('apikeys.activity.rescuedBy', { channel: event.finalChannel || t('apikeys.activity.unknownChannel') })}
                                </Badge>
                              )}
                              {hasAttemptChain && (
                                <details className='group bg-muted/20 rounded border' data-testid='api-key-activity-attempt-chain'>
                                  <summary className='text-muted-foreground cursor-pointer px-2 py-1.5 font-medium select-none'>
                                    {t('apikeys.activity.attemptChain', { count: event.attempts.length })}
                                  </summary>
                                  <ol className='space-y-1 border-t p-2'>
                                    {event.attempts.map((attempt) => {
                                      const attemptResultKind = activityResultKind(attempt.status, attempt.statusCode);
                                      const attemptFailed = attemptResultKind === 'error';
                                      const attemptPending = attemptResultKind === 'pending';
                                      const attemptCategory =
                                        attempt.errorCategory === 'upstream_quota'
                                          ? t('apikeys.activity.upstreamQuota')
                                          : attempt.errorCategory;
                                      const attemptDetail = attemptPending
                                        ? t('apikeys.activity.inProgressDetail', {
                                            status: attempt.status || t('apikeys.activity.unknownStatus'),
                                          })
                                        : attemptFailed
                                          ? [attemptCategory, attempt.errorMessage].filter(Boolean).join(' · ')
                                          : attempt.latencyMs === undefined
                                            ? t('apikeys.activity.success')
                                            : t('apikeys.activity.latency', { value: Math.round(attempt.latencyMs) });

                                      return (
                                        <li
                                          key={`${event.requestId}-${attempt.sequence}`}
                                          className='bg-background/70 rounded border px-2 py-1.5'
                                          data-testid='api-key-activity-attempt'
                                        >
                                          <div className='flex flex-wrap items-center gap-x-2 gap-y-1'>
                                            <span className='font-semibold'>
                                              {t('apikeys.activity.attemptNumber', { number: attempt.sequence })}
                                            </span>
                                            <span>{attempt.channel || t('apikeys.activity.unknownChannel')}</span>
                                            {attempt.apiFormat && <code className='text-muted-foreground'>{attempt.apiFormat}</code>}
                                            <Badge
                                              variant='outline'
                                              className={
                                                attemptFailed
                                                  ? 'border-red-500/30 text-red-700 dark:text-red-400'
                                                  : attemptPending
                                                    ? 'border-amber-500/30 text-amber-700 dark:text-amber-400'
                                                    : 'border-emerald-500/30 text-emerald-700 dark:text-emerald-400'
                                              }
                                            >
                                              {attemptPending
                                                ? t('apikeys.activity.inProgress')
                                                : (attempt.statusCode ??
                                                  t(attemptFailed ? 'apikeys.activity.error' : 'apikeys.activity.success'))}
                                            </Badge>
                                          </div>
                                          <p
                                            className={
                                              attemptFailed
                                                ? 'mt-1 break-words text-red-700 dark:text-red-400'
                                                : attemptPending
                                                  ? 'mt-1 break-words text-amber-700 dark:text-amber-400'
                                                  : 'text-muted-foreground mt-1 break-words'
                                            }
                                          >
                                            {attemptDetail || '—'}
                                          </p>
                                        </li>
                                      );
                                    })}
                                  </ol>
                                </details>
                              )}
                            </div>
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </div>
            )}
          </>
        )}
      </CardContent>
    </Card>
  );
}
