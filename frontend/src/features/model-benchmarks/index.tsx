import { useState } from 'react';
import { ArrowUpRight, BadgeDollarSign, BrainCircuit, ChartNoAxesCombined, CircleAlert, Loader2 } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Header } from '@/components/layout/header';
import { Main } from '@/components/layout/main';

const ARTIFICIAL_ANALYSIS_EMBED_URL = 'https://artificialanalysis.ai/embed/llm-leaderboard';
const ARTIFICIAL_ANALYSIS_LEADERBOARD_URL = 'https://artificialanalysis.ai/leaderboards/models';

export default function ModelBenchmarksPage() {
  const { t } = useTranslation();
  const [frameLoaded, setFrameLoaded] = useState(false);
  const [frameFailed, setFrameFailed] = useState(false);

  return (
    <div className='flex flex-1 flex-col overflow-hidden' data-testid='model-benchmarks-page'>
      <Header fixed>
        <div className='min-w-0'>
          <h2 className='truncate text-xl font-bold tracking-tight'>{t('modelBenchmarks.title')}</h2>
          <p className='text-muted-foreground truncate text-sm'>{t('modelBenchmarks.description')}</p>
        </div>
      </Header>

      <Main fixed className='overflow-y-auto'>
        <div className='mx-auto flex w-full max-w-7xl flex-col gap-5 pb-8'>
          <div className='grid gap-3 md:grid-cols-2'>
            <Card className='gap-3 py-4 shadow-none'>
              <CardContent className='flex items-start gap-3 px-4'>
                <div className='bg-primary/10 text-primary rounded-lg p-2'>
                  <BrainCircuit className='size-5' aria-hidden='true' />
                </div>
                <div>
                  <h3 className='font-semibold'>{t('modelBenchmarks.performance.title')}</h3>
                  <p className='text-muted-foreground mt-1 text-sm leading-5'>{t('modelBenchmarks.performance.description')}</p>
                </div>
              </CardContent>
            </Card>

            <Card className='gap-3 py-4 shadow-none'>
              <CardContent className='flex items-start gap-3 px-4'>
                <div className='bg-primary/10 text-primary rounded-lg p-2'>
                  <BadgeDollarSign className='size-5' aria-hidden='true' />
                </div>
                <div>
                  <h3 className='font-semibold'>{t('modelBenchmarks.cost.title')}</h3>
                  <p className='text-muted-foreground mt-1 text-sm leading-5'>{t('modelBenchmarks.cost.description')}</p>
                </div>
              </CardContent>
            </Card>
          </div>

          <div className='bg-muted/30 text-muted-foreground flex items-start gap-3 rounded-xl border px-4 py-3 text-sm'>
            <CircleAlert className='text-primary mt-0.5 size-4 shrink-0' aria-hidden='true' />
            <p>{t('modelBenchmarks.notice')}</p>
          </div>

          <Card className='gap-0 overflow-hidden py-0 shadow-none'>
            <CardHeader className='gap-2 border-b px-5 py-4 sm:flex sm:flex-row sm:items-center sm:justify-between sm:px-6'>
              <div className='min-w-0'>
                <div className='flex items-center gap-2'>
                  <ChartNoAxesCombined className='text-primary size-5 shrink-0' aria-hidden='true' />
                  <CardTitle>{t('modelBenchmarks.embed.title')}</CardTitle>
                </div>
                <CardDescription className='mt-1'>{t('modelBenchmarks.embed.description')}</CardDescription>
              </div>
              <Button asChild variant='outline' size='sm' className='w-full shrink-0 gap-1.5 sm:w-auto'>
                <a href={ARTIFICIAL_ANALYSIS_LEADERBOARD_URL} target='_blank' rel='noreferrer noopener'>
                  {t('modelBenchmarks.openOfficial')}
                  <ArrowUpRight className='size-4' aria-hidden='true' />
                </a>
              </Button>
            </CardHeader>

            <CardContent className='relative min-h-[720px] p-0'>
              {!frameLoaded && !frameFailed && (
                <div className='bg-muted/20 absolute inset-0 z-10 flex items-center justify-center' role='status'>
                  <div className='text-muted-foreground flex items-center gap-2 text-sm'>
                    <Loader2 className='size-4 animate-spin' aria-hidden='true' />
                    {t('modelBenchmarks.embed.loading')}
                  </div>
                </div>
              )}

              {frameFailed && (
                <div className='flex min-h-[720px] flex-col items-center justify-center gap-4 px-6 text-center'>
                  <CircleAlert className='text-muted-foreground size-8' aria-hidden='true' />
                  <div>
                    <p className='font-semibold'>{t('modelBenchmarks.embed.failed')}</p>
                    <p className='text-muted-foreground mt-1 max-w-xl text-sm'>{t('modelBenchmarks.embed.failedDescription')}</p>
                  </div>
                  <Button asChild>
                    <a href={ARTIFICIAL_ANALYSIS_LEADERBOARD_URL} target='_blank' rel='noreferrer noopener'>
                      {t('modelBenchmarks.openOfficial')}
                    </a>
                  </Button>
                </div>
              )}

              {!frameFailed && (
                <iframe
                  src={ARTIFICIAL_ANALYSIS_EMBED_URL}
                  title={t('modelBenchmarks.embed.frameTitle')}
                  className='h-[78vh] min-h-[720px] w-full border-0 bg-white'
                  loading='lazy'
                  referrerPolicy='strict-origin-when-cross-origin'
                  onLoad={() => setFrameLoaded(true)}
                  onError={() => setFrameFailed(true)}
                  data-testid='artificial-analysis-leaderboard'
                />
              )}
            </CardContent>
          </Card>

          <p className='text-muted-foreground text-xs leading-5'>{t('modelBenchmarks.source')}</p>
        </div>
      </Main>
    </div>
  );
}
