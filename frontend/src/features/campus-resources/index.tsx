import { useEffect, useMemo, useState } from 'react';
import { BookOpenCheck, Check, Copy, KeyRound, Layers3, Loader2, RadioTower, Search, ShieldCheck, UserRound } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';
import { useCopyToClipboard } from '@/hooks/use-copy-to-clipboard';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Skeleton } from '@/components/ui/skeleton';
import { Header } from '@/components/layout/header';
import { Main } from '@/components/layout/main';
import { type CampusResourceChannel, useCampusResources } from './data/resources';

const ALL_API_KEYS = 'all';

function ModelCopyButton({ model }: { model: string }) {
  const { t } = useTranslation();
  const { isCopied, handleCopy } = useCopyToClipboard({ text: model });

  return (
    <button
      type='button'
      className='group bg-background hover:border-primary/40 hover:bg-accent/50 focus-visible:ring-ring/50 flex min-w-0 items-center justify-between gap-3 rounded-lg border px-3 py-2.5 text-left transition-colors focus-visible:ring-[3px] focus-visible:outline-none'
      onClick={handleCopy}
      aria-label={t('resources.models.copy', { model })}
      title={t('resources.models.copy', { model })}
      data-testid='campus-resource-model-copy'
      data-model-name={model}
    >
      <code className='min-w-0 truncate text-xs font-medium sm:text-sm'>{model}</code>
      {isCopied ? (
        <Check className='size-4 shrink-0 text-emerald-600' aria-hidden='true' />
      ) : (
        <Copy className='text-muted-foreground group-hover:text-foreground size-4 shrink-0 transition-colors' aria-hidden='true' />
      )}
    </button>
  );
}

function SummaryCard({ icon: Icon, label, value }: { icon: typeof Layers3; label: string; value: number }) {
  return (
    <Card className='gap-3 py-4 shadow-none'>
      <CardContent className='flex items-center gap-3 px-4'>
        <div className='bg-primary/10 text-primary rounded-lg p-2'>
          <Icon className='size-4' aria-hidden='true' />
        </div>
        <div className='min-w-0'>
          <div className='text-2xl font-semibold tabular-nums'>{value}</div>
          <div className='text-muted-foreground truncate text-xs'>{label}</div>
        </div>
      </CardContent>
    </Card>
  );
}

function ChannelCard({ channel }: { channel: CampusResourceChannel }) {
  const { t, i18n } = useTranslation();
  const providerLabel = t(`channels.providers.${channel.provider}`, {
    defaultValue: t(`channels.types.${channel.provider}`, { defaultValue: channel.provider }),
  });

  const formattedExpiry = useMemo(() => {
    if (!channel.expiresAt) return null;
    const value = new Date(channel.expiresAt);
    if (Number.isNaN(value.getTime())) return channel.expiresAt;

    return new Intl.DateTimeFormat(i18n.language.startsWith('zh') ? 'zh-CN' : 'en-US', {
      dateStyle: 'medium',
      timeStyle: 'short',
    }).format(value);
  }, [channel.expiresAt, i18n.language]);

  return (
    <Card className='gap-4 py-5 shadow-none' data-testid='campus-resource-channel' data-channel-source={channel.source}>
      <CardHeader className='gap-3 px-5'>
        <div className='flex min-w-0 items-start justify-between gap-3'>
          <div className='min-w-0'>
            <CardTitle className='truncate text-base' title={channel.name}>
              {channel.name}
            </CardTitle>
            <CardDescription className='mt-1 flex items-center gap-1.5'>
              <RadioTower className='size-3.5 shrink-0' aria-hidden='true' />
              <span className='truncate'>{providerLabel}</span>
            </CardDescription>
          </div>
          <Badge
            variant='outline'
            className={cn(
              'shrink-0',
              channel.status === 'enabled'
                ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400'
                : 'border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-400'
            )}
          >
            {t(`resources.channels.status.${channel.status}`)}
          </Badge>
        </div>

        <div>
          <Badge variant='secondary'>{t(`resources.channels.source.${channel.source}`)}</Badge>
        </div>
      </CardHeader>

      <CardContent className='flex flex-1 flex-col gap-4 px-5'>
        <p className='text-muted-foreground min-h-10 text-sm leading-5'>{channel.description || t('resources.channels.noDescription')}</p>

        <dl className='mt-auto grid gap-2 border-t pt-4 text-xs'>
          <div className='flex items-center justify-between gap-3'>
            <dt className='text-muted-foreground flex items-center gap-1.5'>
              <UserRound className='size-3.5' aria-hidden='true' />
              {t('resources.channels.contributor')}
            </dt>
            <dd className='min-w-0 truncate text-right font-medium' title={channel.contributor}>
              {channel.contributor}
            </dd>
          </div>
          <div className='flex items-center justify-between gap-3'>
            <dt className='text-muted-foreground'>{t('resources.channels.expiresAt')}</dt>
            <dd className='text-right font-medium'>
              {channel.expiresAt ? <time dateTime={channel.expiresAt}>{formattedExpiry}</time> : t('resources.channels.noExpiry')}
            </dd>
          </div>
          <div className='flex items-center justify-between gap-3'>
            <dt className='text-muted-foreground'>{t('resources.channels.modelCount')}</dt>
            <dd className='font-mono font-medium tabular-nums'>{channel.modelCount}</dd>
          </div>
        </dl>
      </CardContent>
    </Card>
  );
}

function ResourcesLoading() {
  return (
    <div className='flex-1 space-y-5 p-6 md:p-8' data-testid='campus-resources-loading'>
      <div className='space-y-2'>
        <Skeleton className='h-7 w-48' />
        <Skeleton className='h-4 w-full max-w-xl' />
      </div>
      <div className='grid gap-3 sm:grid-cols-3'>
        <Skeleton className='h-24' />
        <Skeleton className='h-24' />
        <Skeleton className='h-24' />
      </div>
      <Skeleton className='h-72' />
    </div>
  );
}

export default function CampusResourcesPage() {
  const { t } = useTranslation();
  const [selectedApiKey, setSelectedApiKey] = useState(ALL_API_KEYS);
  const [modelSearch, setModelSearch] = useState('');
  const { data, isLoading, isFetching, error, refetch } = useCampusResources();

  useEffect(() => {
    if (!data || selectedApiKey === ALL_API_KEYS) return;
    const selectedIndex = Number(selectedApiKey.replace('key-', ''));
    if (!Number.isInteger(selectedIndex) || !data.apiKeys[selectedIndex]) {
      setSelectedApiKey(ALL_API_KEYS);
    }
  }, [data, selectedApiKey]);

  const selectedModels = useMemo(() => {
    if (!data) return [];
    if (selectedApiKey === ALL_API_KEYS) return data.models;

    const selectedIndex = Number(selectedApiKey.replace('key-', ''));
    return data.apiKeys[selectedIndex]?.models ?? data.models;
  }, [data, selectedApiKey]);

  const filteredModels = useMemo(() => {
    const query = modelSearch.trim().toLocaleLowerCase();
    const uniqueModels = [...new Set(selectedModels)].sort((left, right) => left.localeCompare(right));
    if (!query) return uniqueModels;
    return uniqueModels.filter((model) => model.toLocaleLowerCase().includes(query));
  }, [modelSearch, selectedModels]);

  if (isLoading) return <ResourcesLoading />;

  if (error || !data) {
    return (
      <div className='flex flex-1 items-center justify-center p-6' data-testid='campus-resources-error'>
        <Card className='w-full max-w-lg gap-4 text-center shadow-none'>
          <CardHeader>
            <CardTitle>{t('resources.error.title')}</CardTitle>
            <CardDescription>{t('resources.error.description')}</CardDescription>
          </CardHeader>
          <CardContent>
            <Button variant='outline' onClick={() => void refetch()}>
              {t('common.buttons.retry')}
            </Button>
          </CardContent>
        </Card>
      </div>
    );
  }

  const selectedHasNoModels = selectedModels.length === 0;
  const noSearchResults = !selectedHasNoModels && filteredModels.length === 0;

  return (
    <div className='flex flex-1 flex-col overflow-hidden' data-testid='campus-resources-page'>
      <Header fixed>
        <div className='flex min-w-0 flex-1 items-center justify-between gap-4'>
          <div className='min-w-0'>
            <h2 className='truncate text-xl font-bold tracking-tight'>{t('resources.title')}</h2>
            <p className='text-muted-foreground truncate text-sm'>{t('resources.description')}</p>
          </div>
          {isFetching && (
            <div className='text-muted-foreground flex shrink-0 items-center gap-2 text-xs' role='status'>
              <Loader2 className='size-4 animate-spin' aria-hidden='true' />
              <span className='hidden sm:inline'>{t('resources.refreshing')}</span>
            </div>
          )}
        </div>
      </Header>

      <Main fixed className='overflow-y-auto'>
        <div className='mx-auto flex w-full max-w-7xl flex-col gap-5 pb-8'>
          <div className='bg-muted/30 text-muted-foreground flex items-start gap-3 rounded-xl border px-4 py-3 text-sm'>
            <ShieldCheck className='text-primary mt-0.5 size-4 shrink-0' aria-hidden='true' />
            <p>{t('resources.privacyNotice')}</p>
          </div>

          <div className='grid gap-3 sm:grid-cols-3' data-testid='campus-resources-summary'>
            <SummaryCard icon={Layers3} label={t('resources.summary.models')} value={data.models.length} />
            <SummaryCard icon={KeyRound} label={t('resources.summary.apiKeys')} value={data.apiKeys.length} />
            <SummaryCard icon={RadioTower} label={t('resources.summary.channels')} value={data.channels.length} />
          </div>

          <Card className='gap-5 py-5 shadow-none' data-testid='campus-resource-models'>
            <CardHeader className='gap-1 px-5 sm:px-6'>
              <div className='flex items-center gap-2'>
                <BookOpenCheck className='text-primary size-5' aria-hidden='true' />
                <CardTitle>{t('resources.models.title')}</CardTitle>
              </div>
              <CardDescription>{t('resources.models.description')}</CardDescription>
            </CardHeader>

            <CardContent className='space-y-4 px-5 sm:px-6'>
              <div className='flex flex-col gap-3 sm:flex-row'>
                <div className='space-y-1.5 sm:w-72'>
                  <label className='text-muted-foreground text-xs font-medium' htmlFor='resource-api-key-filter'>
                    {t('resources.models.apiKeyFilter')}
                  </label>
                  <Select value={selectedApiKey} onValueChange={setSelectedApiKey} disabled={data.apiKeys.length === 0}>
                    <SelectTrigger id='resource-api-key-filter' className='w-full' data-testid='campus-resource-api-key-filter'>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent align='start'>
                      <SelectItem value={ALL_API_KEYS}>{t('resources.models.allApiKeys', { count: data.models.length })}</SelectItem>
                      {data.apiKeys.map((apiKey, index) => (
                        <SelectItem key={`${apiKey.name}-${index}`} value={`key-${index}`}>
                          {t('resources.models.apiKeyOption', {
                            name: apiKey.name || t('resources.models.unnamedApiKey'),
                            count: apiKey.models.length,
                          })}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>

                <div className='flex-1 space-y-1.5'>
                  <label className='text-muted-foreground text-xs font-medium' htmlFor='resource-model-search'>
                    {t('resources.models.searchLabel')}
                  </label>
                  <div className='relative'>
                    <Search className='text-muted-foreground absolute top-1/2 left-3 size-4 -translate-y-1/2' aria-hidden='true' />
                    <Input
                      id='resource-model-search'
                      type='search'
                      className='pl-9'
                      value={modelSearch}
                      onChange={(event) => setModelSearch(event.target.value)}
                      placeholder={t('resources.models.searchPlaceholder')}
                      data-testid='campus-resource-model-search'
                    />
                  </div>
                </div>
              </div>

              <div className='text-muted-foreground flex items-center justify-between border-t pt-4 text-xs'>
                <span>
                  {t('resources.models.visibleCount', {
                    visible: filteredModels.length,
                    total: new Set(selectedModels).size,
                  })}
                </span>
                <span className='hidden sm:inline'>{t('resources.models.copyHint')}</span>
              </div>

              {selectedHasNoModels || noSearchResults ? (
                <div className='text-muted-foreground flex min-h-36 items-center justify-center rounded-xl border border-dashed px-4 text-center text-sm'>
                  {data.apiKeys.length === 0
                    ? t('resources.models.emptyNoApiKeys')
                    : noSearchResults
                      ? t('resources.models.emptySearch')
                      : t('resources.models.emptyForApiKey')}
                </div>
              ) : (
                <div className='grid gap-2 sm:grid-cols-2 lg:grid-cols-3' data-testid='campus-resource-model-list'>
                  {filteredModels.map((model) => (
                    <ModelCopyButton key={model} model={model} />
                  ))}
                </div>
              )}
            </CardContent>
          </Card>

          <section className='space-y-4' aria-labelledby='campus-resource-channels-title'>
            <div className='flex items-start gap-3'>
              <RadioTower className='text-primary mt-0.5 size-5 shrink-0' aria-hidden='true' />
              <div>
                <h3 id='campus-resource-channels-title' className='font-semibold'>
                  {t('resources.channels.title')}
                </h3>
                <p className='text-muted-foreground text-sm'>{t('resources.channels.description')}</p>
              </div>
            </div>

            {data.channels.length === 0 ? (
              <div className='text-muted-foreground flex min-h-36 items-center justify-center rounded-xl border border-dashed px-4 text-sm'>
                {t('resources.channels.empty')}
              </div>
            ) : (
              <div className='grid gap-4 md:grid-cols-2 xl:grid-cols-3' data-testid='campus-resource-channel-list'>
                {data.channels.map((channel, index) => (
                  <ChannelCard key={`${channel.name}-${channel.provider}-${channel.contributor}-${index}`} channel={channel} />
                ))}
              </div>
            )}
          </section>
        </div>
      </Main>
    </div>
  );
}
