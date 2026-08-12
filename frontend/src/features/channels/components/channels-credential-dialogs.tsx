import { useMemo } from 'react';
import { IconAlertTriangle, IconLoader2 } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { useChannels } from '../context/channels-context';
import { useChannelCredentials } from '../data/channels';
import { Channel } from '../data/schema';
import { ChannelsActionDialog } from './channels-action-dialog';
import { ChannelsTestAPIKeysDialog } from './channels-test-api-keys-dialog';

interface ChannelsCredentialDialogsProps {
  channel: Channel;
}

/**
 * Hosts the dialogs that need a channel's credentials. The list query no longer
 * carries them, so they are fetched on demand here and the dialogs only mount
 * once the fetch succeeded: mounting them earlier would freeze empty keys into
 * the edit form, and saving that form overwrites the stored keys.
 *
 * The caller keys this component by channel id and unmounts it when the row is
 * cleared, which drops the fetched keys from the query cache (`gcTime: 0`).
 */
export function ChannelsCredentialDialogs({ channel }: ChannelsCredentialDialogsProps) {
  const { t } = useTranslation();
  const { open, setOpen, setCurrentRow } = useChannels();

  const isCredentialDialogOpen = open === 'edit' || open === 'duplicate' || open === 'viewModels' || open === 'testAPIKeys';

  const {
    data: credentials,
    isSuccess,
    isError,
    isFetching,
    refetch,
  } = useChannelCredentials(channel.id, { enabled: isCredentialDialogOpen });

  const closeDialog = () => {
    setOpen(null);
    setTimeout(() => {
      setCurrentRow(null);
    }, 500);
  };

  const handleOpenChange = (isOpen: boolean) => {
    if (!isOpen) {
      closeDialog();
    }
  };

  // `credentials` is null when the viewer may not read this channel's secrets.
  const channelWithCredentials = useMemo(() => ({ ...channel, credentials: credentials ?? undefined }), [channel, credentials]);

  if (!isSuccess) {
    return (
      <Dialog open={isCredentialDialogOpen} onOpenChange={handleOpenChange}>
        <DialogContent className='sm:max-w-md'>
          {isError ? (
            <>
              <DialogHeader>
                <DialogTitle className='flex items-center gap-2'>
                  <IconAlertTriangle className='text-destructive h-5 w-5' />
                  {t('channels.dialogs.credentials.errorTitle')}
                </DialogTitle>
                <DialogDescription>{t('channels.dialogs.credentials.errorDescription')}</DialogDescription>
              </DialogHeader>
              <DialogFooter>
                <Button variant='outline' onClick={closeDialog}>
                  {t('common.close')}
                </Button>
                <Button onClick={() => refetch()} disabled={isFetching}>
                  {isFetching && <IconLoader2 className='mr-2 h-4 w-4 animate-spin' />}
                  {t('common.buttons.retry')}
                </Button>
              </DialogFooter>
            </>
          ) : (
            <>
              <DialogHeader>
                <DialogTitle>{t('channels.dialogs.credentials.loadingTitle')}</DialogTitle>
                <DialogDescription>{t('channels.dialogs.credentials.loadingDescription')}</DialogDescription>
              </DialogHeader>
              <div className='flex items-center justify-center py-8'>
                <IconLoader2 className='text-muted-foreground h-6 w-6 animate-spin' />
              </div>
            </>
          )}
        </DialogContent>
      </Dialog>
    );
  }

  return (
    <>
      <ChannelsActionDialog
        key={`channel-edit-${channel.id}`}
        open={open === 'edit'}
        onOpenChange={(isOpen) => {
          if (isOpen) {
            setOpen('edit');
          } else {
            closeDialog();
          }
        }}
        currentRow={channelWithCredentials}
      />

      <ChannelsActionDialog
        key={`channel-duplicate-${channel.id}`}
        open={open === 'duplicate'}
        onOpenChange={(isOpen) => {
          if (isOpen) {
            setOpen('duplicate');
          } else {
            closeDialog();
          }
        }}
        duplicateFromRow={channelWithCredentials}
      />

      <ChannelsActionDialog
        key={`channel-view-models-${channel.id}`}
        open={open === 'viewModels'}
        onOpenChange={(isOpen) => {
          if (isOpen) {
            setOpen('viewModels');
          } else {
            closeDialog();
          }
        }}
        currentRow={channelWithCredentials}
        showModelsPanel={true}
      />

      <ChannelsTestAPIKeysDialog
        key={`channel-test-api-keys-${channel.id}`}
        open={open === 'testAPIKeys'}
        onOpenChange={handleOpenChange}
        currentRow={channel}
        credentials={credentials}
      />
    </>
  );
}
