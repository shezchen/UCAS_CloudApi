'use client';

import React, { useState, useEffect } from 'react';
import { Loader2, Plus, Save, Trash2 } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Switch } from '@/components/ui/switch';
import { AutoCompleteSelect } from '@/components/auto-complete-select';
import { usePermissions } from '@/hooks/usePermissions';
import { useSystemContext } from '../context/system-context';
import { currencyCodes } from '../data/currencies';
import {
  type CampusFriendLink,
  useCampusFriendLinks,
  useGeneralSettings,
  useUpdateCampusFriendLinks,
  useUpdateGeneralSettings,
  useUserAgentPassThroughSettings,
  useUpdateUserAgentPassThroughSettings,
  usePassThroughSettings,
  useUpdatePassThroughSettings,
} from '../data/system';
import { GMTTimeZoneOptions } from '../data/timezones';

export function GeneralSettings() {
  const { t } = useTranslation();
  const { isOwner } = usePermissions();
  const { data: settings, isLoading: isLoadingSettings } = useGeneralSettings();
  const updateSettings = useUpdateGeneralSettings();
  const { isLoading, setIsLoading } = useSystemContext();

  // User-Agent Pass-Through settings
  const { data: uaSettings, isLoading: isLoadingUASettings } = useUserAgentPassThroughSettings();
  const updateUASettings = useUpdateUserAgentPassThroughSettings();
  const [uaPassThroughEnabled, setUaPassThroughEnabled] = useState(false);

  // Pass-Through (request/response body) settings
  const { data: ptSettings, isLoading: isLoadingPTSettings } = usePassThroughSettings();
  const updatePTSettings = useUpdatePassThroughSettings();
  const [passThroughEnabled, setPassThroughEnabled] = useState(false);

  const [currencyCode, setCurrencyCode] = useState('USD');
  const [timezone, setTimezone] = useState('UTC');

  const currencyItems = React.useMemo(
    () =>
      currencyCodes.map((code) => ({
        value: code,
        label: t(`currencies.${code}`),
      })),
    [t]
  );

  const timezoneItems = React.useMemo(() => GMTTimeZoneOptions, []);

  // Update local state when settings are loaded
  useEffect(() => {
    if (settings) {
      setCurrencyCode(settings.currencyCode || 'USD');
      setTimezone(settings.timezone || 'UTC');
    }
  }, [settings]);

  // Update UA pass-through state when loaded
  useEffect(() => {
    if (uaSettings) {
      setUaPassThroughEnabled(uaSettings.enabled);
    }
  }, [uaSettings]);

  // Update pass-through state when loaded
  useEffect(() => {
    if (ptSettings) {
      setPassThroughEnabled(ptSettings.enabled);
    }
  }, [ptSettings]);

  const handleSave = async () => {
    setIsLoading(true);
    try {
      await updateSettings.mutateAsync({
        currencyCode: currencyCode.trim(),
        timezone: timezone.trim(),
      });
    } finally {
      setIsLoading(false);
    }
  };

  const handleUAPassThroughChange = async (enabled: boolean) => {
    const previousValue = uaPassThroughEnabled;
    setUaPassThroughEnabled(enabled);
    try {
      await updateUASettings.mutateAsync({ enabled });
    } catch {
      // Revert state on error
      setUaPassThroughEnabled(previousValue);
    }
  };

  const handlePassThroughChange = async (enabled: boolean) => {
    const previousValue = passThroughEnabled;
    setPassThroughEnabled(enabled);
    try {
      await updatePTSettings.mutateAsync({ enabled });
    } catch {
      // Revert state on error
      setPassThroughEnabled(previousValue);
    }
  };

  const hasChanges = settings
    ? settings.currencyCode !== currencyCode || settings.timezone !== timezone
    : false;

  if (isLoadingSettings) {
    return (
      <div className='flex h-32 items-center justify-center'>
        <Loader2 className='h-6 w-6 animate-spin' />
        <span className='text-muted-foreground ml-2'>{t('common.loading')}</span>
      </div>
    );
  }

  return (
    <div className='space-y-6'>
      <Card>
        <CardHeader>
          <CardTitle>{t('system.general.title')}</CardTitle>
          <CardDescription>{t('system.general.description')}</CardDescription>
        </CardHeader>
        <CardContent className='space-y-6'>
          <div className='space-y-2'>
            <Label htmlFor='currency-code'>{t('system.general.currencyCode.label')}</Label>
            <div className='max-w-md'>
              <AutoCompleteSelect
                selectedValue={currencyCode}
                onSelectedValueChange={setCurrencyCode}
                items={currencyItems}
                placeholder={t('system.general.currencyCode.placeholder')}
                isLoading={isLoadingSettings}
              />
            </div>
            <div className='text-muted-foreground text-sm'>{t('system.general.currencyCode.description')}</div>
          </div>

          <div className='space-y-2'>
            <Label htmlFor='timezone'>{t('system.general.timezone.label')}</Label>
            <div className='max-w-md'>
              <AutoCompleteSelect
                selectedValue={timezone}
                onSelectedValueChange={setTimezone}
                items={timezoneItems}
                placeholder={t('system.general.timezone.placeholder')}
                isLoading={isLoadingSettings}
              />
            </div>
            <div className='text-muted-foreground text-sm'>{t('system.general.timezone.description')}</div>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t('system.passThroughGroup.title')}</CardTitle>
          <CardDescription>{t('system.passThroughGroup.description')}</CardDescription>
        </CardHeader>
        <CardContent className='space-y-4'>
          <div className='flex items-center justify-between'>
            <div className='space-y-0.5'>
              <Label htmlFor='ua-pass-through'>{t('system.userAgentPassThrough.label')}</Label>
              <div className='text-muted-foreground text-sm'>{t('system.userAgentPassThrough.helpText')}</div>
            </div>
            <Switch
              id='ua-pass-through'
              checked={uaPassThroughEnabled}
              onCheckedChange={handleUAPassThroughChange}
              disabled={isLoadingUASettings || updateUASettings.isPending}
            />
          </div>
          <div className='flex items-center justify-between'>
            <div className='space-y-0.5'>
              <Label htmlFor='pass-through'>{t('system.passThrough.label')}</Label>
              <div className='text-muted-foreground text-sm'>{t('system.passThrough.helpText')}</div>
            </div>
            <Switch
              id='pass-through'
              checked={passThroughEnabled}
              onCheckedChange={handlePassThroughChange}
              disabled={isLoadingPTSettings || updatePTSettings.isPending}
            />
          </div>
        </CardContent>
      </Card>

      {isOwner && <CampusFriendLinksSettings />}

      {hasChanges && (
        <div className='flex justify-end'>
          <Button onClick={handleSave} disabled={isLoading || updateSettings.isPending} className='min-w-[100px]'>
            {isLoading || updateSettings.isPending ? (
              <>
                <Loader2 className='mr-2 h-4 w-4 animate-spin' />
                {t('system.buttons.saving')}
              </>
            ) : (
              <>
                <Save className='mr-2 h-4 w-4' />
                {t('system.buttons.save')}
              </>
            )}
          </Button>
        </div>
      )}
    </div>
  );
}

function isSafeFriendLinkURL(value: string) {
  try {
    const parsed = new URL(value);
    return (parsed.protocol === 'https:' || parsed.protocol === 'http:') && Boolean(parsed.hostname) && !parsed.username && !parsed.password;
  } catch {
    return false;
  }
}

function CampusFriendLinksSettings() {
  const { t } = useTranslation();
  const { isLoading, setIsLoading } = useSystemContext();
  const { data: savedFriendLinks = [], isLoading: isLoadingFriendLinks } = useCampusFriendLinks();
  const updateFriendLinks = useUpdateCampusFriendLinks();
  const [friendLinks, setFriendLinks] = useState<CampusFriendLink[]>([]);

  useEffect(() => {
    setFriendLinks(savedFriendLinks.map((link) => ({ ...link, description: link.description ?? '' })));
  }, [savedFriendLinks]);

  const hasChanges = JSON.stringify(friendLinks) !== JSON.stringify(savedFriendLinks);
  const isSaving = isLoading || updateFriendLinks.isPending;

  const updateFriendLink = (index: number, field: keyof CampusFriendLink, value: string) => {
    setFriendLinks((current) =>
      current.map((link, currentIndex) => (currentIndex === index ? { ...link, [field]: value } : link))
    );
  };

  const addFriendLink = () => {
    setFriendLinks((current) => [...current, { name: '', url: '', description: '' }]);
  };

  const removeFriendLink = (index: number) => {
    setFriendLinks((current) => current.filter((_, currentIndex) => currentIndex !== index));
  };

  const saveFriendLinks = async () => {
    const normalized = friendLinks.map((link) => ({
      name: link.name.trim(),
      url: link.url.trim(),
      description: link.description.trim(),
    }));

    if (normalized.some((link) => link.name === '' || link.url === '')) {
      toast.error(t('system.friendLinks.validation.required'));
      return;
    }

    if (normalized.some((link) => !isSafeFriendLinkURL(link.url))) {
      toast.error(t('system.friendLinks.validation.httpUrl'));
      return;
    }

    setIsLoading(true);
    try {
      await updateFriendLinks.mutateAsync(normalized);
      setFriendLinks(normalized);
    } finally {
      setIsLoading(false);
    }
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('system.friendLinks.title')}</CardTitle>
        <CardDescription>{t('system.friendLinks.description')}</CardDescription>
      </CardHeader>
      <CardContent className='space-y-4'>
        {isLoadingFriendLinks ? (
          <div className='flex h-16 items-center text-sm text-muted-foreground'>
            <Loader2 className='mr-2 h-4 w-4 animate-spin' />
            {t('common.loading')}
          </div>
        ) : (
          <>
            {friendLinks.length === 0 ? (
              <p className='text-sm text-muted-foreground'>{t('system.friendLinks.empty')}</p>
            ) : (
              <div className='space-y-3'>
                {friendLinks.map((link, index) => (
                  <div
                    key={`${link.url}-${index}`}
                    className='grid gap-3 md:grid-cols-[minmax(0,1fr)_minmax(0,2fr)_minmax(0,1fr)_auto] md:items-center'
                  >
                    <div className='space-y-1'>
                      <Label className='sr-only' htmlFor={`friend-link-name-${index}`}>
                        {t('system.friendLinks.name')}
                      </Label>
                      <Input
                        id={`friend-link-name-${index}`}
                        value={link.name}
                        onChange={(event) => updateFriendLink(index, 'name', event.target.value)}
                        placeholder={t('system.friendLinks.namePlaceholder')}
                        disabled={isSaving}
                      />
                    </div>
                    <div className='space-y-1'>
                      <Label className='sr-only' htmlFor={`friend-link-url-${index}`}>
                        {t('system.friendLinks.url')}
                      </Label>
                      <Input
                        id={`friend-link-url-${index}`}
                        type='url'
                        value={link.url}
                        onChange={(event) => updateFriendLink(index, 'url', event.target.value)}
                        placeholder={t('system.friendLinks.urlPlaceholder')}
                        disabled={isSaving}
                      />
                    </div>
                    <div className='space-y-1'>
                      <Label className='sr-only' htmlFor={`friend-link-description-${index}`}>
                        {t('system.friendLinks.linkDescription')}
                      </Label>
                      <Input
                        id={`friend-link-description-${index}`}
                        value={link.description}
                        onChange={(event) => updateFriendLink(index, 'description', event.target.value)}
                        placeholder={t('system.friendLinks.linkDescriptionPlaceholder')}
                        disabled={isSaving}
                        maxLength={500}
                      />
                    </div>
                    <Button
                      type='button'
                      variant='outline'
                      size='icon'
                      onClick={() => removeFriendLink(index)}
                      disabled={isSaving}
                      aria-label={t('system.friendLinks.remove')}
                    >
                      <Trash2 className='h-4 w-4' />
                    </Button>
                  </div>
                ))}
              </div>
            )}

            <Button type='button' variant='outline' onClick={addFriendLink} disabled={isSaving}>
              <Plus className='mr-2 h-4 w-4' />
              {t('system.friendLinks.add')}
            </Button>

            {hasChanges && (
              <div className='flex justify-end'>
                <Button type='button' onClick={saveFriendLinks} disabled={isSaving} className='min-w-[100px]'>
                  {isSaving ? (
                    <>
                      <Loader2 className='mr-2 h-4 w-4 animate-spin' />
                      {t('system.buttons.saving')}
                    </>
                  ) : (
                    <>
                      <Save className='mr-2 h-4 w-4' />
                      {t('system.buttons.save')}
                    </>
                  )}
                </Button>
              </div>
            )}
          </>
        )}
      </CardContent>
    </Card>
  );
}
