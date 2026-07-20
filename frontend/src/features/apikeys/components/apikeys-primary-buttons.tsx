import { IconPlus, IconTemplate } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/button';
import { usePermissions } from '@/hooks/usePermissions';
import { useApiKeysContext } from '../context/apikeys-context';

export function ApiKeysPrimaryButtons() {
  const { t } = useTranslation();
  const { openDialog } = useApiKeysContext();
  const { isProjectOwner } = usePermissions();

  return (
    <div className='flex gap-2'>
      {isProjectOwner && (
        <Button variant='outline' size='sm' onClick={() => openDialog('profileTemplates')}>
          <IconTemplate className='mr-2 h-4 w-4' />
          {t('apikeys.profileTemplates.button')}
        </Button>
      )}
      <Button onClick={() => openDialog('create')} size='sm'>
        <IconPlus className='mr-2 h-4 w-4' />
        {t('apikeys.createApiKey')}
      </Button>
    </div>
  );
}
