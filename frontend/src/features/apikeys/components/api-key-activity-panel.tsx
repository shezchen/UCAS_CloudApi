import { useMemo, useState } from 'react';
import { Activity, CircleCheck, CircleX, Clock3, Loader2 } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Badge } from '@/components/ui/badge';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { formatNumber } from '@/utils/format-number';
import { useCampusApiActivity } from '../data/apikeys';

const ALL_API_KEYS = 'all';

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
              <div className='rounded-lg border bg-muted/20 px-3 py-2'>
                <div className='text-lg font-semibold tabular-nums'>{formatNumber(totals.effectiveTokens)}</div>
                <div className='text-muted-foreground text-xs'>{t('apikeys.activity.effectiveTokens')}</div>
              </div>
              <div className='rounded-lg border bg-muted/20 px-3 py-2'>
                <div className='text-lg font-semibold tabular-nums'>{formatNumber(totals.inputTokens)}</div>
                <div className='text-muted-foreground text-xs'>{t('apikeys.columns.inputTokens')}</div>
              </div>
              <div className='rounded-lg border bg-muted/20 px-3 py-2'>
                <div className='text-lg font-semibold tabular-nums'>{formatNumber(totals.cachedReadTokens)}</div>
                <div className='text-muted-foreground text-xs'>{t('apikeys.activity.cachedReadTokens')}</div>
              </div>
              <div className='rounded-lg border bg-muted/20 px-3 py-2'>
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
                  <TableHeader className='sticky top-0 z-10 bg-background'>
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
                      const normalizedStatus = event.status.toLocaleLowerCase();
                      const isError =
                        ['error', 'failed', 'canceled', 'cancelled'].includes(normalizedStatus) ||
                        (event.statusCode !== undefined && event.statusCode >= 400);
                      const detail = isError
                        ? [event.errorCategory, event.errorMessage].filter(Boolean).join(' · ')
                        : event.latencyMs === undefined
                          ? t('apikeys.activity.success')
                          : t('apikeys.activity.latency', { value: Math.round(event.latencyMs) });

                      return (
                        <TableRow key={event.requestId} data-testid='api-key-activity-event-row'>
                          <TableCell className='whitespace-nowrap text-xs'>
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
                                  : 'border-emerald-500/30 text-emerald-700 dark:text-emerald-400'
                              }
                            >
                              {event.statusCode ?? t(isError ? 'apikeys.activity.error' : 'apikeys.activity.success')}
                            </Badge>
                          </TableCell>
                          <TableCell className='max-w-96 text-xs'>
                            <span className={isError ? 'text-red-700 dark:text-red-400' : 'text-muted-foreground'} title={detail}>
                              {detail || '—'}
                            </span>
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
