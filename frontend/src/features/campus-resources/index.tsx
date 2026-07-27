import { useEffect, useMemo, useState } from 'react';
import { Link } from '@tanstack/react-router';
import {
  BookOpenCheck,
  CalendarDays,
  Check,
  CircleAlert,
  CircleCheckBig,
  CircleHelp,
  CircleX,
  Copy,
  Gauge,
  HeartHandshake,
  KeyRound,
  Layers3,
  Loader2,
  Play,
  RadioTower,
  RefreshCw,
  Search,
  Settings2,
  ShieldCheck,
  UserRound,
  WalletCards,
} from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { cn } from '@/lib/utils';
import { useCopyToClipboard } from '@/hooks/use-copy-to-clipboard';
import { usePermissions } from '@/hooks/usePermissions';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Skeleton } from '@/components/ui/skeleton';
import { Switch } from '@/components/ui/switch';
import { Header } from '@/components/layout/header';
import { Main } from '@/components/layout/main';
import {
  type CampusManagedChannel,
  type CampusDonationBenefits,
  type CampusModelDetail,
  type CampusResourceChannel,
  type CampusUsageOverview,
  useCampusResources,
  useChannelModelCapabilities,
  useProbeCampusChannel,
  useUpdateChannelModelCapability,
} from './data/resources';

const ALL_API_KEYS = 'all';
const MAX_CAPABILITY_TOKENS = 10_000_000;
const DEFAULT_DAILY_EFFECTIVE_TOKEN_LIMIT = 16_000_000;
const DEFAULT_WEEKLY_EFFECTIVE_TOKEN_LIMIT = 64_000_000;

function ModelCapabilityBadge({ label, enabled }: { label: string; enabled: boolean }) {
  const { t } = useTranslation();

  return (
    <Badge
      variant='outline'
      className={cn(
        'font-normal',
        enabled
          ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400'
          : 'text-muted-foreground border-border bg-muted/40'
      )}
    >
      {label}: {t(enabled ? 'resources.models.capability.supported' : 'resources.models.capability.unsupported')}
    </Badge>
  );
}

function ModelCard({ model, details }: { model: string; details?: CampusModelDetail }) {
  const { t, i18n } = useTranslation();
  const { isCopied, handleCopy } = useCopyToClipboard({ text: model });
  const numberFormatter = useMemo(() => new Intl.NumberFormat(i18n.language.startsWith('zh') ? 'zh-CN' : 'en-US'), [i18n.language]);

  return (
    <div className='bg-background flex min-w-0 flex-col gap-3 rounded-lg border p-3'>
      <button
        type='button'
        className='group hover:text-primary focus-visible:ring-ring/50 flex min-w-0 items-center justify-between gap-3 rounded-sm text-left transition-colors focus-visible:ring-[3px] focus-visible:outline-none'
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

      {details && (
        <div className='flex flex-wrap gap-1.5 text-[11px]'>
          <Badge variant='secondary' className='font-normal'>
            {t('resources.models.capability.source')}: {t(`resources.models.source.${details.source}`, { defaultValue: details.source })}
          </Badge>
          {details.variesByAPIKey && (
            <Badge variant='secondary' className='font-normal'>
              {t('resources.models.capability.variesByAPIKey')}
            </Badge>
          )}
          <ModelCapabilityBadge label={t('resources.models.capability.vision')} enabled={details.vision} />
          <ModelCapabilityBadge label={t('resources.models.capability.toolCall')} enabled={details.toolCall} />
          <ModelCapabilityBadge label={t('resources.models.capability.reasoning')} enabled={details.reasoning} />
          <Badge variant='outline' className='font-normal'>
            {t('resources.models.capability.context')}:{' '}
            {details.contextLength > 0 ? numberFormatter.format(details.contextLength) : t('resources.models.capability.unknown')}
          </Badge>
          <Badge variant='outline' className='font-normal'>
            {t('resources.models.capability.output')}:{' '}
            {details.maxOutputTokens === undefined
              ? t('resources.models.capability.unknown')
              : numberFormatter.format(details.maxOutputTokens)}
          </Badge>
        </div>
      )}
    </div>
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

const channelHealthPresentation = {
  healthy: {
    icon: CircleCheckBig,
    className: 'border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400',
  },
  degraded: {
    icon: CircleAlert,
    className: 'border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-400',
  },
  unhealthy: {
    icon: CircleX,
    className: 'border-red-500/30 bg-red-500/10 text-red-700 dark:text-red-400',
  },
  recovering: {
    icon: RefreshCw,
    className: 'border-blue-500/30 bg-blue-500/10 text-blue-700 dark:text-blue-400',
  },
  unknown: {
    icon: CircleHelp,
    className: 'text-muted-foreground border-border bg-muted/40',
  },
} as const;

function ChannelCard({ channel }: { channel: CampusResourceChannel }) {
  const { t, i18n } = useTranslation();
  const probeChannel = useProbeCampusChannel();
  const providerLabel = t(`channels.providers.${channel.provider}`, {
    defaultValue: t(`channels.types.${channel.provider}`, { defaultValue: channel.provider }),
  });
  const healthState = channel.health?.state ?? 'unknown';
  const healthPresentation = channelHealthPresentation[healthState];
  const HealthIcon = healthPresentation.icon;
  const recentSuccessRate = channel.health?.recentSuccessRate;
  const successRatePercent =
    recentSuccessRate === undefined || channel.health?.recentRequestCount === 0
      ? undefined
      : Math.min(100, Math.max(0, recentSuccessRate <= 1 ? recentSuccessRate * 100 : recentSuccessRate));
  const recentSuccessCount =
    successRatePercent === undefined || channel.health?.recentRequestCount === undefined
      ? undefined
      : Math.round((successRatePercent / 100) * channel.health.recentRequestCount);

  const formattedExpiry = useMemo(() => {
    if (!channel.expiresAt) return null;
    const value = new Date(channel.expiresAt);
    if (Number.isNaN(value.getTime())) return channel.expiresAt;

    return new Intl.DateTimeFormat(i18n.language.startsWith('zh') ? 'zh-CN' : 'en-US', {
      dateStyle: 'medium',
      timeStyle: 'short',
    }).format(value);
  }, [channel.expiresAt, i18n.language]);

  const formattedLastChecked = useMemo(() => {
    if (!channel.health?.lastCheckedAt) return null;
    const value = new Date(channel.health.lastCheckedAt);
    if (Number.isNaN(value.getTime())) return channel.health.lastCheckedAt;

    return new Intl.DateTimeFormat(i18n.language.startsWith('zh') ? 'zh-CN' : 'en-US', {
      dateStyle: 'short',
      timeStyle: 'short',
    }).format(value);
  }, [channel.health?.lastCheckedAt, i18n.language]);

  const probe = () => {
    if (!channel.id) return;

    probeChannel.mutate(channel.id, {
      onSuccess: (result) =>
        result?.success === false
          ? toast.error(t('resources.channels.probe.failed'))
          : toast.success(t('resources.channels.probe.success')),
      onError: () => toast.error(t('resources.channels.probe.error')),
    });
  };

  return (
    <Card
      className={cn('gap-4 py-5 shadow-none', channel.source === 'donated' && 'border-primary/30 bg-primary/[0.025]')}
      data-testid='campus-resource-channel'
      data-channel-source={channel.source}
    >
      <CardHeader className='gap-3 px-5'>
        <div className='flex min-w-0 items-start justify-between gap-3'>
          <div className='min-w-0'>
            <CardTitle className='truncate text-base' title={channel.name}>
              {channel.name}
            </CardTitle>
            <CardDescription className='text-foreground/80 mt-1 flex items-center gap-1.5 font-medium'>
              <UserRound className='text-primary size-3.5 shrink-0' aria-hidden='true' />
              <span className='truncate' title={channel.contributor}>
                {t('resources.channels.providedBy', { contributor: channel.contributor })}
              </span>
            </CardDescription>
            <CardDescription className='mt-1 flex items-center gap-1.5'>
              <RadioTower className='size-3.5 shrink-0' aria-hidden='true' />
              <span className='truncate'>{providerLabel}</span>
            </CardDescription>
          </div>
          <div className='flex shrink-0 flex-col items-end gap-2'>
            <Badge
              variant='outline'
              className={cn(
                channel.status === 'enabled'
                  ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400'
                  : 'border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-400'
              )}
            >
              {t(`resources.channels.status.${channel.status}`)}
            </Badge>
            <Badge
              variant='outline'
              className={cn('gap-1', healthPresentation.className)}
              data-testid='campus-channel-health-state'
              data-health-state={healthState}
            >
              <HealthIcon className={cn('size-3', healthState === 'recovering' && 'animate-spin')} aria-hidden='true' />
              {t(`resources.channels.health.state.${healthState}`)}
            </Badge>
          </div>
        </div>

        <div className='flex items-center justify-between gap-3'>
          <Badge variant='secondary'>{t(`resources.channels.source.${channel.source}`)}</Badge>
          {channel.canProbe && channel.id && (
            <Button
              type='button'
              variant='outline'
              size='sm'
              className='h-8 gap-1.5'
              onClick={probe}
              disabled={probeChannel.isPending}
              aria-label={t('resources.channels.probe.action', { channel: channel.name })}
              title={t('resources.channels.probe.hint')}
              data-testid='campus-channel-probe'
              data-channel-id={channel.id}
            >
              {probeChannel.isPending ? (
                <Loader2 className='size-3.5 animate-spin' aria-hidden='true' />
              ) : (
                <Play className='size-3.5 fill-current' aria-hidden='true' />
              )}
              {t(probeChannel.isPending ? 'resources.channels.probe.testing' : 'resources.channels.probe.test')}
            </Button>
          )}
        </div>
      </CardHeader>

      <CardContent className='flex flex-1 flex-col gap-4 px-5'>
        <p className='text-muted-foreground min-h-10 text-sm leading-5'>{channel.description || t('resources.channels.noDescription')}</p>

        <dl className='grid gap-2 rounded-lg border bg-muted/20 p-3 text-xs' data-testid='campus-channel-health-details'>
          <div className='flex items-center justify-between gap-3'>
            <dt className='text-muted-foreground'>{t('resources.channels.health.successRate')}</dt>
            <dd className='font-mono font-medium tabular-nums'>
              {successRatePercent === undefined ? t('resources.channels.health.unknown') : `${successRatePercent.toFixed(1)}%`}
            </dd>
          </div>
          <div className='flex items-center justify-between gap-3'>
            <dt className='text-muted-foreground'>{t('resources.channels.health.successCount')}</dt>
            <dd className='font-mono font-medium tabular-nums'>
              {recentSuccessCount === undefined || channel.health?.recentRequestCount === undefined
                ? t('resources.channels.health.unknown')
                : t('resources.channels.health.countValue', {
                    success: recentSuccessCount,
                    total: channel.health.recentRequestCount,
                  })}
            </dd>
          </div>
          <div className='flex items-center justify-between gap-3'>
            <dt className='text-muted-foreground'>{t('resources.channels.health.lastChecked')}</dt>
            <dd className='text-right font-medium'>
              {channel.health?.lastCheckedAt ? (
                <time dateTime={channel.health.lastCheckedAt}>{formattedLastChecked}</time>
              ) : (
                t('resources.channels.health.neverChecked')
              )}
            </dd>
          </div>
          <div className='flex items-center justify-between gap-3'>
            <dt className='text-muted-foreground'>{t('resources.channels.health.failureCategory')}</dt>
            <dd className='max-w-[65%] truncate text-right font-medium' title={channel.health?.lastFailureCategory}>
              {channel.health?.lastFailureCategory
                ? t(`resources.channels.health.failure.${channel.health.lastFailureCategory}`, {
                    defaultValue: channel.health.lastFailureCategory,
                  })
                : t('resources.channels.health.none')}
            </dd>
          </div>
        </dl>

        <dl className='mt-auto grid gap-2 border-t pt-4 text-xs'>
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

function formatTokenCount(value: number | undefined, locale: string) {
  if (value === undefined) return '—';
  return new Intl.NumberFormat(locale, { notation: 'compact', maximumFractionDigits: 2 }).format(value);
}

function EffectiveTokenQuotaCard({
  icon: Icon,
  title,
  period,
  fallbackLimit,
  resetDescription,
  locale,
}: {
  icon: typeof CalendarDays;
  title: string;
  period: CampusUsageOverview['daily'];
  fallbackLimit: number;
  resetDescription: string;
  locale: string;
}) {
  const { t } = useTranslation();
  const limit = period?.limit ?? fallbackLimit;
  const used = period?.used;
  const remaining = period?.remaining ?? (used === undefined ? undefined : Math.max(limit - used, 0));
  const percent = used === undefined || limit === 0 ? undefined : Math.min(100, (used / limit) * 100);

  return (
    <Card className='gap-3 py-4 shadow-none' data-testid='campus-effective-token-quota'>
      <CardHeader className='gap-1 px-4'>
        <div className='flex items-center gap-2'>
          <div className='bg-primary/10 text-primary rounded-lg p-2'>
            <Icon className='size-4' aria-hidden='true' />
          </div>
          <CardTitle className='text-sm'>{title}</CardTitle>
        </div>
      </CardHeader>
      <CardContent className='space-y-3 px-4'>
        <div>
          <div className='text-xl font-semibold tabular-nums'>
            {formatTokenCount(used, locale)} / {formatTokenCount(limit, locale)}
          </div>
          <p className='text-muted-foreground mt-1 text-xs'>
            {t('resources.usage.remaining', { value: formatTokenCount(remaining, locale) })}
          </p>
        </div>
        <div className='bg-muted h-2 overflow-hidden rounded-full' aria-hidden='true'>
          <div className='bg-primary h-full rounded-full transition-[width]' style={{ width: `${percent ?? 0}%` }} />
        </div>
        <p className='text-muted-foreground text-xs'>{resetDescription}</p>
      </CardContent>
    </Card>
  );
}

function UsageAndBenefits({
  usageOverview,
  donationBenefits,
}: {
  usageOverview?: CampusUsageOverview;
  donationBenefits?: CampusDonationBenefits;
}) {
  const { t, i18n } = useTranslation();
  const locale = i18n.language.startsWith('zh') ? 'zh-CN' : 'en-US';
  const benefits = usageOverview?.donations.length ? usageOverview.donations : (donationBenefits?.channels ?? []);

  return (
    <section className='space-y-4' aria-labelledby='campus-usage-overview-title' data-testid='campus-usage-overview'>
      <div className='flex items-start gap-3'>
        <Gauge className='text-primary mt-0.5 size-5 shrink-0' aria-hidden='true' />
        <div>
          <h3 id='campus-usage-overview-title' className='font-semibold'>
            {t('resources.usage.title')}
          </h3>
          <p className='text-muted-foreground text-sm'>{t('resources.usage.description')}</p>
        </div>
      </div>

      <div className='grid gap-3 md:grid-cols-3'>
        <EffectiveTokenQuotaCard
          icon={CalendarDays}
          title={t('resources.usage.daily.title')}
          period={usageOverview?.daily}
          fallbackLimit={DEFAULT_DAILY_EFFECTIVE_TOKEN_LIMIT}
          resetDescription={t('resources.usage.daily.reset')}
          locale={locale}
        />
        <EffectiveTokenQuotaCard
          icon={CalendarDays}
          title={t('resources.usage.weekly.title')}
          period={usageOverview?.weekly}
          fallbackLimit={DEFAULT_WEEKLY_EFFECTIVE_TOKEN_LIMIT}
          resetDescription={t('resources.usage.weekly.reset')}
          locale={locale}
        />
        <Card className='gap-3 border-primary/25 bg-primary/[0.025] py-4 shadow-none' data-testid='campus-token-wallet'>
          <CardHeader className='gap-1 px-4'>
            <div className='flex items-center gap-2'>
              <div className='bg-primary/10 text-primary rounded-lg p-2'>
                <WalletCards className='size-4' aria-hidden='true' />
              </div>
              <CardTitle className='text-sm'>{t('resources.usage.wallet.title')}</CardTitle>
            </div>
          </CardHeader>
          <CardContent className='space-y-2 px-4'>
            <div className='text-xl font-semibold tabular-nums'>
              {formatTokenCount(usageOverview?.wallet?.balance, locale)}
            </div>
            <p className='text-muted-foreground text-xs'>{t('resources.usage.wallet.description')}</p>
            <dl className='grid gap-1 border-t pt-2 text-xs'>
              <div className='flex justify-between gap-3'>
                <dt className='text-muted-foreground'>{t('resources.usage.wallet.credited')}</dt>
                <dd className='font-mono tabular-nums'>
                  {formatTokenCount(usageOverview?.wallet?.lifetimeEarned, locale)}
                </dd>
              </div>
              <div className='flex justify-between gap-3'>
                <dt className='text-muted-foreground'>{t('resources.usage.wallet.spent')}</dt>
                <dd className='font-mono tabular-nums'>
                  {formatTokenCount(usageOverview?.wallet?.lifetimeSpent, locale)}
                </dd>
              </div>
            </dl>
          </CardContent>
        </Card>
      </div>

      {benefits.length > 0 && (
        <Card className='gap-3 py-4 shadow-none' data-testid='campus-donation-benefits'>
          <CardHeader className='gap-1 px-4 sm:px-5'>
            <CardTitle className='flex items-center gap-2 text-base'>
              <HeartHandshake className='text-primary size-4' aria-hidden='true' />
              {t('resources.usage.benefits.title')}
            </CardTitle>
            <CardDescription>{t('resources.usage.benefits.description')}</CardDescription>
          </CardHeader>
          <CardContent className='grid gap-2 px-4 sm:px-5'>
            {benefits.map((benefit, index) => (
              <div
                key={benefit.channelId ?? `${benefit.name}-${index}`}
                className='grid gap-2 rounded-lg border px-3 py-3 text-sm sm:grid-cols-[minmax(0,1fr)_auto_auto_auto] sm:items-center'
                data-testid='campus-donation-benefit-row'
              >
                <span className='min-w-0 truncate font-medium' title={benefit.name}>
                  {benefit.name}
                </span>
                <span className='text-muted-foreground'>
                  {t('resources.usage.benefits.used', {
                    value: formatTokenCount(benefit.effectiveTokens, locale),
                  })}
                </span>
                <span className='text-muted-foreground'>
                  {t('resources.usage.benefits.eligible', {
                    value: formatTokenCount(benefit.rewardEligibleTokens, locale),
                  })}
                </span>
                <span className='font-medium text-emerald-700 dark:text-emerald-400'>
                  {t('resources.usage.benefits.credited', {
                    value: formatTokenCount(benefit.creditTokens, locale),
                  })}
                </span>
              </div>
            ))}
          </CardContent>
        </Card>
      )}
    </section>
  );
}

function GettingStartedCard() {
  const { t } = useTranslation();

  const useSteps = [
    t('resources.gettingStarted.use.step1'),
    t('resources.gettingStarted.use.step2'),
    t('resources.gettingStarted.use.step3'),
  ];

  return (
    <section className='grid gap-4 lg:grid-cols-[minmax(0,1.45fr)_minmax(0,1fr)]' aria-label={t('resources.gettingStarted.title')}>
      <Card className='gap-4 border-primary/30 bg-primary/[0.045] py-5 shadow-none' data-testid='campus-resources-getting-started'>
        <CardHeader className='gap-2 px-5 sm:px-6'>
          <div className='flex items-start gap-3'>
            <div className='bg-primary text-primary-foreground rounded-lg p-2'>
              <KeyRound className='size-5' aria-hidden='true' />
            </div>
            <div className='min-w-0'>
              <CardTitle>{t('resources.gettingStarted.use.title')}</CardTitle>
              <CardDescription className='mt-1'>{t('resources.gettingStarted.use.description')}</CardDescription>
            </div>
          </div>
        </CardHeader>
        <CardContent className='space-y-4 px-5 sm:px-6'>
          <ol className='grid gap-2 text-sm sm:grid-cols-3'>
            {useSteps.map((step, index) => (
              <li key={step} className='bg-background/70 flex items-start gap-2 rounded-lg border px-3 py-2.5'>
                <span className='bg-primary/10 text-primary flex size-5 shrink-0 items-center justify-center rounded-full text-xs font-semibold'>
                  {index + 1}
                </span>
                <span>{step}</span>
              </li>
            ))}
          </ol>
          <Button asChild>
            <Link to='/project/api-keys'>{t('resources.gettingStarted.use.action')}</Link>
          </Button>
        </CardContent>
      </Card>

      <Card className='gap-4 py-5 shadow-none' data-testid='campus-resources-donation-entry'>
        <CardHeader className='gap-2 px-5'>
          <div className='flex items-start gap-3'>
            <div className='bg-muted text-primary rounded-lg p-2'>
              <HeartHandshake className='size-5' aria-hidden='true' />
            </div>
            <div className='min-w-0'>
              <CardTitle className='text-base'>{t('resources.gettingStarted.donate.title')}</CardTitle>
              <CardDescription className='mt-1'>{t('resources.gettingStarted.donate.description')}</CardDescription>
            </div>
          </div>
        </CardHeader>
        <CardContent className='px-5'>
          <Button asChild variant='outline'>
            <Link to='/channels'>{t('resources.gettingStarted.donate.action')}</Link>
          </Button>
        </CardContent>
      </Card>
    </section>
  );
}

function ChannelModelCapabilityDialog({
  channel,
  open,
  onOpenChange,
}: {
  channel: CampusManagedChannel;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  const updateCapability = useUpdateChannelModelCapability();
  const firstModel = channel.models[0];
  const [selectedModelID, setSelectedModelID] = useState(firstModel?.id ?? '');
  const [vision, setVision] = useState(firstModel?.vision ?? true);
  const [toolCall, setToolCall] = useState(firstModel?.toolCall ?? true);
  const [reasoning, setReasoning] = useState(firstModel?.reasoning ?? true);
  const [contextLength, setContextLength] = useState(String(firstModel?.contextLength ?? 1_000_000));
  const [maxOutputTokens, setMaxOutputTokens] = useState(
    firstModel?.maxOutputTokens === undefined ? '' : String(firstModel.maxOutputTokens)
  );

  const selectedModel = channel.models.find((model) => model.id === selectedModelID);
  const parsedContextLength = Number(contextLength);
  const parsedMaxOutputTokens = maxOutputTokens.trim() === '' ? undefined : Number(maxOutputTokens);
  const contextLengthIsValid =
    Number.isSafeInteger(parsedContextLength) && parsedContextLength > 0 && parsedContextLength <= MAX_CAPABILITY_TOKENS;
  const maxOutputTokensIsValid =
    parsedMaxOutputTokens === undefined ||
    (Number.isSafeInteger(parsedMaxOutputTokens) &&
      parsedMaxOutputTokens > 0 &&
      parsedMaxOutputTokens <= MAX_CAPABILITY_TOKENS &&
      parsedMaxOutputTokens <= parsedContextLength);

  const selectModel = (modelID: string) => {
    const model = channel.models.find((candidate) => candidate.id === modelID);
    if (!model) return;

    setSelectedModelID(modelID);
    setVision(model.vision);
    setToolCall(model.toolCall);
    setReasoning(model.reasoning);
    setContextLength(String(model.contextLength));
    setMaxOutputTokens(model.maxOutputTokens === undefined ? '' : String(model.maxOutputTokens));
  };

  const closeAfterSuccess = (message: string) => {
    toast.success(message);
    onOpenChange(false);
  };

  const saveOverride = () => {
    if (!selectedModel || !contextLengthIsValid || !maxOutputTokensIsValid) return;

    updateCapability.mutate(
      {
        channelID: channel.id,
        modelID: selectedModel.id,
        override: {
          vision,
          toolCall,
          reasoning,
          contextLength: parsedContextLength,
          ...(parsedMaxOutputTokens === undefined ? {} : { maxOutputTokens: parsedMaxOutputTokens }),
        },
      },
      {
        onSuccess: () => closeAfterSuccess(t('resources.manage.saveSuccess')),
        onError: () => toast.error(t('resources.manage.updateError')),
      }
    );
  };

  const restoreAutomatic = () => {
    if (!selectedModel) return;

    updateCapability.mutate(
      {
        channelID: channel.id,
        modelID: selectedModel.id,
        override: null,
      },
      {
        onSuccess: () => closeAfterSuccess(t('resources.manage.resetSuccess')),
        onError: () => toast.error(t('resources.manage.updateError')),
      }
    );
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className='sm:max-w-xl' data-testid='campus-channel-model-capability-dialog'>
        <DialogHeader>
          <DialogTitle>{t('resources.manage.dialogTitle', { channel: channel.name })}</DialogTitle>
          <DialogDescription>{t('resources.manage.dialogDescription')}</DialogDescription>
        </DialogHeader>

        <div className='space-y-5'>
          <div className='space-y-2'>
            <Label htmlFor='campus-capability-model'>{t('resources.manage.model')}</Label>
            <Select value={selectedModelID} onValueChange={selectModel}>
              <SelectTrigger id='campus-capability-model' className='w-full' data-testid='campus-capability-model-select'>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {channel.models.map((model) => (
                  <SelectItem key={model.id} value={model.id}>
                    {model.id}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          {selectedModel && (
            <>
              <div className='bg-muted/40 flex flex-wrap items-center gap-2 rounded-lg border px-3 py-2 text-xs'>
                <span className='text-muted-foreground'>{t('resources.manage.source')}</span>
                <Badge variant='secondary'>
                  {t(`resources.models.source.${selectedModel.source}`, { defaultValue: selectedModel.source })}
                </Badge>
                <Badge variant={selectedModel.overridden ? 'default' : 'outline'}>
                  {t(selectedModel.overridden ? 'resources.manage.overridden' : 'resources.manage.automatic')}
                </Badge>
              </div>

              <div className='grid gap-3 sm:grid-cols-3'>
                <div className='flex items-center justify-between gap-3 rounded-lg border p-3'>
                  <Label htmlFor='campus-capability-vision'>{t('resources.models.capability.vision')}</Label>
                  <Switch id='campus-capability-vision' checked={vision} onCheckedChange={setVision} />
                </div>
                <div className='flex items-center justify-between gap-3 rounded-lg border p-3'>
                  <Label htmlFor='campus-capability-tool-call'>{t('resources.models.capability.toolCall')}</Label>
                  <Switch id='campus-capability-tool-call' checked={toolCall} onCheckedChange={setToolCall} />
                </div>
                <div className='flex items-center justify-between gap-3 rounded-lg border p-3'>
                  <Label htmlFor='campus-capability-reasoning'>{t('resources.models.capability.reasoning')}</Label>
                  <Switch id='campus-capability-reasoning' checked={reasoning} onCheckedChange={setReasoning} />
                </div>
              </div>

              <div className='grid gap-4 sm:grid-cols-2'>
                <div className='space-y-2'>
                  <Label htmlFor='campus-capability-context'>{t('resources.manage.contextLength')}</Label>
                  <Input
                    id='campus-capability-context'
                    type='number'
                    min={1}
                    max={MAX_CAPABILITY_TOKENS}
                    step={1}
                    value={contextLength}
                    onChange={(event) => setContextLength(event.target.value)}
                    aria-invalid={!contextLengthIsValid}
                  />
                  {!contextLengthIsValid && <p className='text-destructive text-xs'>{t('resources.manage.invalidContext')}</p>}
                </div>
                <div className='space-y-2'>
                  <Label htmlFor='campus-capability-output'>{t('resources.manage.maxOutputTokens')}</Label>
                  <Input
                    id='campus-capability-output'
                    type='number'
                    min={1}
                    max={contextLengthIsValid ? parsedContextLength : MAX_CAPABILITY_TOKENS}
                    step={1}
                    value={maxOutputTokens}
                    onChange={(event) => setMaxOutputTokens(event.target.value)}
                    placeholder={t('resources.manage.outputOptional')}
                    aria-invalid={!maxOutputTokensIsValid}
                  />
                  {!maxOutputTokensIsValid && <p className='text-destructive text-xs'>{t('resources.manage.invalidOutput')}</p>}
                </div>
              </div>
            </>
          )}
        </div>

        <DialogFooter className='sm:justify-between'>
          <Button
            type='button'
            variant='outline'
            onClick={restoreAutomatic}
            disabled={!selectedModel?.overridden || updateCapability.isPending}
          >
            {t('resources.manage.restoreAuto')}
          </Button>
          <div className='flex flex-col-reverse gap-2 sm:flex-row'>
            <Button type='button' variant='ghost' onClick={() => onOpenChange(false)} disabled={updateCapability.isPending}>
              {t('common.buttons.cancel')}
            </Button>
            <Button
              type='button'
              onClick={saveOverride}
              disabled={!selectedModel || !contextLengthIsValid || !maxOutputTokensIsValid || updateCapability.isPending}
            >
              {updateCapability.isPending && <Loader2 className='size-4 animate-spin' aria-hidden='true' />}
              {t('resources.manage.save')}
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function ChannelModelManagement() {
  const { t } = useTranslation();
  const [editingChannel, setEditingChannel] = useState<CampusManagedChannel | null>(null);
  const { data, isLoading, isFetching, error, refetch } = useChannelModelCapabilities();

  return (
    <section className='space-y-4' aria-labelledby='campus-channel-model-management-title' data-testid='campus-channel-model-capabilities'>
      <div className='flex items-start gap-3'>
        <Settings2 className='text-primary mt-0.5 size-5 shrink-0' aria-hidden='true' />
        <div className='min-w-0 flex-1'>
          <div className='flex items-center gap-2'>
            <h3 id='campus-channel-model-management-title' className='font-semibold'>
              {t('resources.manage.title')}
            </h3>
            {isFetching && !isLoading && <Loader2 className='text-muted-foreground size-3.5 animate-spin' aria-hidden='true' />}
          </div>
          <p className='text-muted-foreground text-sm'>{t('resources.manage.description')}</p>
        </div>
      </div>

      {isLoading ? (
        <div className='grid gap-3 sm:grid-cols-2'>
          <Skeleton className='h-24' />
          <Skeleton className='h-24' />
        </div>
      ) : error || !data ? (
        <div className='text-muted-foreground flex min-h-28 flex-col items-center justify-center gap-3 rounded-xl border border-dashed px-4 text-center text-sm'>
          <span>{t('resources.manage.loadError')}</span>
          <Button type='button' variant='outline' size='sm' onClick={() => void refetch()}>
            {t('common.buttons.retry')}
          </Button>
        </div>
      ) : data.channels.length === 0 ? (
        <div className='text-muted-foreground flex min-h-28 items-center justify-center rounded-xl border border-dashed px-4 text-center text-sm'>
          {t('resources.manage.empty')}
        </div>
      ) : (
        <div className='grid gap-3 sm:grid-cols-2 xl:grid-cols-3' data-testid='campus-channel-model-management-list'>
          {data.channels.map((channel) => (
            <Card key={channel.id} className='gap-3 py-4 shadow-none'>
              <CardHeader className='gap-1 px-4'>
                <CardTitle className='truncate text-base' title={channel.name}>
                  {channel.name}
                </CardTitle>
                <CardDescription>{t('resources.manage.channelModelCount', { count: channel.models.length })}</CardDescription>
              </CardHeader>
              <CardContent className='px-4'>
                <Button
                  type='button'
                  variant='outline'
                  size='sm'
                  className='w-full'
                  onClick={() => setEditingChannel(channel)}
                  disabled={channel.models.length === 0}
                  data-testid='campus-channel-model-edit'
                >
                  <Settings2 className='size-4' aria-hidden='true' />
                  {channel.models.length === 0 ? t('resources.manage.noModels') : t('resources.manage.edit')}
                </Button>
              </CardContent>
            </Card>
          ))}
        </div>
      )}

      {editingChannel && (
        <ChannelModelCapabilityDialog
          key={editingChannel.id}
          channel={editingChannel}
          open
          onOpenChange={(nextOpen) => {
            if (!nextOpen) setEditingChannel(null);
          }}
        />
      )}
    </section>
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
  const { isOwner } = usePermissions();
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

  const selectedModelDetails = useMemo(() => {
    if (!data) return [];
    if (selectedApiKey === ALL_API_KEYS) return data.modelDetails;

    const selectedIndex = Number(selectedApiKey.replace('key-', ''));
    return data.apiKeys[selectedIndex]?.modelDetails ?? [];
  }, [data, selectedApiKey]);

  const modelDetailsByID = useMemo(() => new Map(selectedModelDetails.map((details) => [details.id, details])), [selectedModelDetails]);

  const filteredModels = useMemo(() => {
    const query = modelSearch.trim().toLocaleLowerCase();
    const uniqueModels = [...new Set(selectedModels)].sort((left, right) => left.localeCompare(right));
    if (!query) return uniqueModels;
    return uniqueModels.filter((model) => model.toLocaleLowerCase().includes(query));
  }, [modelSearch, selectedModels]);

  const donatedChannels = useMemo(() => data?.channels.filter((channel) => channel.source === 'donated') ?? [], [data?.channels]);
  const projectChannels = useMemo(() => data?.channels.filter((channel) => channel.source === 'project') ?? [], [data?.channels]);

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
          {!isOwner && <GettingStartedCard />}

          <div className='bg-muted/30 text-muted-foreground flex items-start gap-3 rounded-xl border px-4 py-3 text-sm'>
            <ShieldCheck className='text-primary mt-0.5 size-4 shrink-0' aria-hidden='true' />
            <p>{t('resources.privacyNotice')}</p>
          </div>

          <div className='grid gap-3 sm:grid-cols-3' data-testid='campus-resources-summary'>
            <SummaryCard icon={Layers3} label={t('resources.summary.models')} value={data.models.length} />
            <SummaryCard icon={KeyRound} label={t('resources.summary.apiKeys')} value={data.apiKeys.length} />
            <SummaryCard icon={RadioTower} label={t('resources.summary.channels')} value={data.channels.length} />
          </div>

          <UsageAndBenefits usageOverview={data.usageOverview} donationBenefits={data.donationBenefits} />

          {donatedChannels.length > 0 && (
            <section className='space-y-4' aria-labelledby='campus-resource-donations-title'>
              <div className='flex items-start gap-3'>
                <HeartHandshake className='text-primary mt-0.5 size-5 shrink-0' aria-hidden='true' />
                <div>
                  <h3 id='campus-resource-donations-title' className='font-semibold'>
                    {t('resources.donations.title')}
                  </h3>
                  <p className='text-muted-foreground text-sm'>{t('resources.donations.description')}</p>
                </div>
              </div>

              <div className='grid gap-4 md:grid-cols-2 xl:grid-cols-3' data-testid='campus-resource-donated-channel-list'>
                {donatedChannels.map((channel, index) => (
                  <ChannelCard key={`${channel.name}-${channel.provider}-${channel.contributor}-${index}`} channel={channel} />
                ))}
              </div>
            </section>
          )}

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
                    <ModelCard key={model} model={model} details={modelDetailsByID.get(model)} />
                  ))}
                </div>
              )}
            </CardContent>
          </Card>

          {!isOwner && <ChannelModelManagement />}

          <section className='space-y-4' aria-labelledby='campus-resource-channels-title'>
            <div className='flex items-start gap-3'>
              <RadioTower className='text-primary mt-0.5 size-5 shrink-0' aria-hidden='true' />
              <div>
                <h3 id='campus-resource-channels-title' className='font-semibold'>
                  {t('resources.channels.projectTitle')}
                </h3>
                <p className='text-muted-foreground text-sm'>{t('resources.channels.projectDescription')}</p>
              </div>
            </div>

            {projectChannels.length === 0 ? (
              <div className='text-muted-foreground flex min-h-36 items-center justify-center rounded-xl border border-dashed px-4 text-sm'>
                {data.channels.length === 0 ? t('resources.channels.empty') : t('resources.channels.noProjectChannels')}
              </div>
            ) : (
              <div className='grid gap-4 md:grid-cols-2 xl:grid-cols-3' data-testid='campus-resource-channel-list'>
                {projectChannels.map((channel, index) => (
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
