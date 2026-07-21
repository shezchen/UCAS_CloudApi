'use client';

import React from 'react';
import { Loader2, Save, ShieldCheck } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { usePermissions } from '@/hooks/usePermissions';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { useUpdateUserDailyQuotaSettings, useUserDailyQuotaSettings } from '../data/users';

export function UserDailyQuotaSettingsCard() {
  const { t } = useTranslation();
  const { isOwner } = usePermissions();
  const { data: settings, isLoading } = useUserDailyQuotaSettings({ enabled: isOwner });
  const updateSettings = useUpdateUserDailyQuotaSettings();
  const [dailyTokenLimit, setDailyTokenLimit] = React.useState('');

  React.useEffect(() => {
    if (settings) {
      setDailyTokenLimit(String(settings.dailyTokenLimit));
    }
  }, [settings]);

  if (!isOwner) {
    return null;
  }

  const parsedDailyTokenLimit = Number(dailyTokenLimit);
  const isValid = dailyTokenLimit.trim() !== '' && Number.isSafeInteger(parsedDailyTokenLimit) && parsedDailyTokenLimit >= 0;
  const hasChanges = settings != null && parsedDailyTokenLimit !== settings.dailyTokenLimit;
  const isSaving = updateSettings.isPending;

  const handleSave = async () => {
    if (!isValid) {
      return;
    }

    await updateSettings.mutateAsync({ dailyTokenLimit: parsedDailyTokenLimit });
  };

  return (
    <Card className='border-primary/40 bg-primary/5'>
      <CardHeader>
        <CardTitle className='flex items-center gap-2'>
          <ShieldCheck className='text-primary h-5 w-5' />
          {t('users.dailyQuota.title')}
        </CardTitle>
        <CardDescription>{t('users.dailyQuota.description')}</CardDescription>
      </CardHeader>
      <CardContent className='flex flex-col gap-4 sm:flex-row sm:items-end'>
        <div className='grid flex-1 gap-2'>
          <Label htmlFor='user-daily-token-limit'>{t('users.dailyQuota.limitLabel')}</Label>
          <Input
            id='user-daily-token-limit'
            type='number'
            min={0}
            step={1}
            inputMode='numeric'
            value={dailyTokenLimit}
            onChange={(event) => setDailyTokenLimit(event.target.value)}
            disabled={isLoading || isSaving}
          />
          <p className='text-muted-foreground text-sm'>{t('users.dailyQuota.limitHint')}</p>
        </div>
        <Button onClick={handleSave} disabled={isLoading || isSaving || !isValid || !hasChanges}>
          {isLoading || isSaving ? (
            <>
              <Loader2 className='mr-2 h-4 w-4 animate-spin' />
              {t('users.dailyQuota.saving')}
            </>
          ) : (
            <>
              <Save className='mr-2 h-4 w-4' />
              {t('users.dailyQuota.save')}
            </>
          )}
        </Button>
      </CardContent>
    </Card>
  );
}
