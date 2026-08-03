'use client';

import React, { useState, useEffect, useCallback } from 'react';
import { CircleCheck, Loader2 } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Separator } from '@/components/ui/separator';
import { Switch } from '@/components/ui/switch';
import { useRetryPolicy, useUpdateRetryPolicy, type RetryPolicyInput } from '../data/system';

export function RetrySettings() {
  const { t } = useTranslation();
  const { data: retryPolicy, isLoading } = useRetryPolicy();
  const updateRetryPolicy = useUpdateRetryPolicy();

  const [formData, setFormData] = useState<RetryPolicyInput>({
    enabled: true,
    maxChannelRetries: 3,
    maxSingleChannelRetries: 0,
    retryDelayMs: 0,
    streamFirstEventTimeoutSeconds: 0,
    nonStreamResponseTimeoutSeconds: 0,
    loadBalancerStrategy: 'round-robin',
    emptyResponseDetection: true,
    upstreamErrorPolicy: {
      mode: 'passthrough',
      customMessage: '',
    },
    autoDisableChannel: {
      enabled: false,
      statuses: [],
    },
  });

  useEffect(() => {
    if (retryPolicy) {
      setFormData({
        enabled: retryPolicy.enabled,
        maxChannelRetries: retryPolicy.maxChannelRetries,
        maxSingleChannelRetries: 0,
        retryDelayMs: 0,
        streamFirstEventTimeoutSeconds: retryPolicy.streamFirstEventTimeoutSeconds,
        nonStreamResponseTimeoutSeconds: retryPolicy.nonStreamResponseTimeoutSeconds,
        loadBalancerStrategy: 'round-robin',
        emptyResponseDetection: true,
        upstreamErrorPolicy: {
          mode: 'passthrough',
          customMessage: '',
        },
        autoDisableChannel: {
          enabled: false,
          statuses: [],
        },
      });
    }
  }, [retryPolicy]);

  const handleInputChange = useCallback((field: keyof RetryPolicyInput, value: string | boolean | number) => {
    setFormData((prev) => ({
      ...prev,
      [field]: value,
    }));
  }, []);

  const handleSubmit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      await updateRetryPolicy.mutateAsync({
        ...formData,
        loadBalancerStrategy: 'round-robin',
        maxSingleChannelRetries: 0,
        retryDelayMs: 0,
        emptyResponseDetection: true,
        upstreamErrorPolicy: { mode: 'passthrough', customMessage: '' },
        autoDisableChannel: { enabled: false, statuses: [] },
      });
    },
    [updateRetryPolicy, formData]
  );

  if (isLoading) {
    return (
      <div className='flex items-center justify-center p-8'>
        <Loader2 className='h-8 w-8 animate-spin' />
      </div>
    );
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('system.retry.title')}</CardTitle>
        <CardDescription>{t('system.retry.description')}</CardDescription>
      </CardHeader>
      <CardContent>
        <form onSubmit={handleSubmit} className='space-y-6'>
          {/* Enable/Disable Retry */}
          <div className='flex items-center justify-between' id='retry-enabled-switch'>
            <div className='space-y-0.5'>
              <Label htmlFor='retry-enabled' className='text-base'>
                {t('system.retry.enabled.label')}
              </Label>
              <div className='text-muted-foreground text-sm'>{t('system.retry.enabled.description')}</div>
            </div>
            <Switch id='retry-enabled' checked={formData.enabled} onCheckedChange={(checked) => handleInputChange('enabled', checked)} />
          </div>

          <Separator />

          <div className='border-primary/20 bg-primary/[0.04] space-y-2 rounded-lg border p-4' data-testid='unified-routing-summary'>
            <div className='flex items-center gap-2 text-sm font-semibold'>
              <CircleCheck className='text-primary size-4' aria-hidden='true' />
              {t('system.retry.unifiedRouting.title')}
            </div>
            <p className='text-muted-foreground text-sm leading-5'>{t('system.retry.unifiedRouting.description')}</p>
            <ul className='text-muted-foreground list-disc space-y-1 pl-5 text-xs leading-5'>
              <li>{t('system.retry.unifiedRouting.fairness')}</li>
              <li>{t('system.retry.unifiedRouting.availability')}</li>
              <li>{t('system.retry.unifiedRouting.failover')}</li>
            </ul>
          </div>

          <Separator />

          {/* Retry Configuration - Only show when enabled */}
          {formData.enabled && (
            <div className='space-y-4'>
              {/* Max Channel Retries */}
              <div className='space-y-2' id='retry-max-retries'>
                <Label htmlFor='max-channel-retries'>{t('system.retry.maxChannelRetries.label')}</Label>
                <div className='text-muted-foreground mb-2 text-sm'>{t('system.retry.maxChannelRetries.description')}</div>
                <Input
                  id='max-channel-retries'
                  type='number'
                  min='0'
                  max='10'
                  value={formData.maxChannelRetries}
                  onChange={(e) => handleInputChange('maxChannelRetries', parseInt(e.target.value) || 0)}
                  className='w-32'
                />
                <div className='text-muted-foreground text-xs'>
                  {t('system.retry.maxChannelRetries.totalAttempts', { count: (formData.maxChannelRetries ?? 0) + 1 })}
                </div>
              </div>

              {/* Response Timeouts */}
              <div className='grid gap-4 md:grid-cols-2'>
                <div className='space-y-2'>
                  <Label htmlFor='stream-first-event-timeout'>{t('system.retry.streamFirstEventTimeoutSeconds.label')}</Label>
                  <div className='text-muted-foreground mb-2 text-sm'>{t('system.retry.streamFirstEventTimeoutSeconds.description')}</div>
                  <div className='flex items-center space-x-2'>
                    <Input
                      id='stream-first-event-timeout'
                      type='number'
                      min='0'
                      max='600'
                      value={formData.streamFirstEventTimeoutSeconds}
                      onChange={(e) => handleInputChange('streamFirstEventTimeoutSeconds', parseInt(e.target.value) || 0)}
                      className='w-32'
                    />
                    <span className='text-muted-foreground text-sm'>s</span>
                  </div>
                </div>

                <div className='space-y-2'>
                  <Label htmlFor='non-stream-response-timeout'>{t('system.retry.nonStreamResponseTimeoutSeconds.label')}</Label>
                  <div className='text-muted-foreground mb-2 text-sm'>{t('system.retry.nonStreamResponseTimeoutSeconds.description')}</div>
                  <div className='flex items-center space-x-2'>
                    <Input
                      id='non-stream-response-timeout'
                      type='number'
                      min='0'
                      max='600'
                      value={formData.nonStreamResponseTimeoutSeconds}
                      onChange={(e) => handleInputChange('nonStreamResponseTimeoutSeconds', parseInt(e.target.value) || 0)}
                      className='w-32'
                    />
                    <span className='text-muted-foreground text-sm'>s</span>
                  </div>
                </div>
              </div>
            </div>
          )}

          <Separator />

          {/* Submit Button */}
          <div className='flex justify-end'>
            <Button type='submit' disabled={updateRetryPolicy.isPending} className='min-w-24'>
              {updateRetryPolicy.isPending ? <Loader2 className='h-4 w-4 animate-spin' /> : t('common.buttons.save')}
            </Button>
          </div>
        </form>
      </CardContent>
    </Card>
  );
}
