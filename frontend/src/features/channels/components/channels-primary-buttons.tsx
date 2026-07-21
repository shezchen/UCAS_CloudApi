import { IconPlus, IconUpload, IconArrowsSort, IconSettings, IconScale } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from '@tanstack/react-router';
import { Button } from '@/components/ui/button';
import { usePermissions } from '@/hooks/usePermissions';
import { useChannels } from '../context/channels-context';

export function ChannelsPrimaryButtons() {
  const { t } = useTranslation();
  const { isOwner } = usePermissions();
  const navigate = useNavigate();
  const { setOpen } = useChannels();

  return (
    <div className='flex gap-2 overflow-x-auto md:overflow-x-visible'>
      {isOwner && (
        <>
          <Button
            variant='outline'
            className='shrink-0 space-x-1'
            onClick={() => navigate({ to: '/system', search: { tab: 'retry' } })}
          >
            <span>{t('channels.loadBalancingStrategy')}</span> <IconScale size={18} />
          </Button>

          <Button variant='outline' className='shrink-0 space-x-1' onClick={() => setOpen('channelSettings')}>
            <span>{t('channels.actions.settings')}</span> <IconSettings size={18} />
          </Button>

          <Button variant='outline' className='shrink-0 space-x-1' onClick={() => setOpen('bulkImport')}>
            <span>{t('channels.importChannels', '批量导入')}</span> <IconUpload size={18} />
          </Button>

          <Button variant='outline' className='shrink-0 space-x-1' onClick={() => setOpen('bulkOrdering')}>
            <span>{t('channels.orderChannels')}</span> <IconArrowsSort size={18} />
          </Button>
        </>
      )}

      <Button className='shrink-0 space-x-1' onClick={() => setOpen('add')} data-testid='add-channel-button'>
        <span>{t(isOwner ? 'channels.actions.addManagedChannel' : 'channels.donation.primaryCTA')}</span> <IconPlus size={18} />
      </Button>
    </div>
  );
}
