import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';

import { useQueryClient } from '@tanstack/react-query';
import { Button } from '@/components/ui/button';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { graphqlRequest } from '@/gql/graphql';
import { UNLINK_OIDC_IDENTITY_MUTATION } from '@/gql/users';
import { authApi } from '@/lib/api-client';
import { Link as LinkIcon, Unlink } from 'lucide-react';

interface OidcManagementProps {
  providers: any[];
}

export default function OidcManagement({ providers }: OidcManagementProps) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [unlinkTarget, setUnlinkTarget] = useState<{ identityId: string; providerName: string } | null>(null);
  const [isUnlinking, setIsUnlinking] = useState(false);

  const handleLink = async (providerId: string) => {
    try {
      const res = await authApi.getOIDCLinkAuthorizeURL(providerId);
      if (res.data && res.data.url) {
        window.location.href = res.data.url;
      }
    } catch (error: any) {
      toast.error(t('security.oidc.linkError') + error.message);
    }
  };

  const handleUnlink = async () => {
    if (!unlinkTarget) return;

    setIsUnlinking(true);
    try {
      await graphqlRequest(UNLINK_OIDC_IDENTITY_MUTATION, { id: unlinkTarget.identityId });
      toast.success(t('security.oidc.unlinkSuccess'));
      // Invalidate providers query and me query to refresh UI
      queryClient.invalidateQueries({ queryKey: ['oidc-providers'] });
      queryClient.invalidateQueries({ queryKey: ['me'] });
      setUnlinkTarget(null);
    } catch (error: any) {
      toast.error(t('security.oidc.unlinkError') + error.message);
    } finally {
      setIsUnlinking(false);
    }
  };

  return (
    <div className='space-y-4'>
      <div>
        <h3 className='text-lg font-medium'>{t('security.oidc.title')}</h3>
        <p className='text-muted-foreground text-sm'>
          {t('security.oidc.description')}
        </p>
      </div>

      {providers.length > 0 && (
        <div className='mt-4 space-y-4'>
          <div className='grid gap-4 grid-cols-1 md:grid-cols-2'>
            {providers.map((p: any) => {
              const isInactive = p.active === false;
              const providerId = p.id || p.name;
              const providerLabel = p.display_name || p.name;

              return (
              <div
                key={providerId}
                className={`flex items-center justify-between rounded-lg border bg-card p-4 shadow-sm transition-all hover:shadow-md ${isInactive ? 'border-2 border-destructive' : ''}`}
                title={isInactive ? t('common.status.inactiveRetry') : undefined}
              >
                <div className='flex items-center gap-3'>
                  {p.icon_url && (
                    <img src={p.icon_url} alt={providerLabel} className='w-8 h-8 object-contain rounded' />
                  )}
                  <div className='flex flex-col'>
                    <span className='font-semibold text-foreground'>{providerLabel}</span>
                    {isInactive ? (
                      <span className='text-xs font-medium text-destructive'>{t('common.status.inactiveRetry')}</span>
                    ) : p.is_linked && (
                      <span className='text-xs text-muted-foreground truncate max-w-[150px]'>
                        {p.linked_email}
                      </span>
                    )}
                  </div>
                </div>
                
                {p.is_linked ? (
                  <Button 
                    variant='ghost' 
                    size='sm' 
                    className='h-9 text-destructive hover:text-destructive hover:bg-destructive/10 transition-colors'
                    onClick={() =>
                      setUnlinkTarget({
                        identityId: p.linked_identity_id.toString(),
                        providerName: providerLabel,
                      })
                    }
                    type='button'
                  >
                    <Unlink className='w-4 h-4 mr-2' />
                    {t('common.unlink')}
                  </Button>
                ) : (
                  <Button 
                    variant='outline' 
                    size='sm' 
                    className='h-9 border-primary/20 hover:border-primary hover:bg-primary/5 transition-colors text-primary'
                    onClick={() => handleLink(providerId)}
                    type='button'
                  >
                    <LinkIcon className='w-4 h-4 mr-2' />
                    {t('common.link')}
                  </Button>
                )}
              </div>
              );
            })}
          </div>
        </div>
      )}

      <ConfirmDialog
        open={Boolean(unlinkTarget)}
        onOpenChange={(open) => {
          if (!open && !isUnlinking) {
            setUnlinkTarget(null);
          }
        }}
        title={t('security.oidc.confirmUnlinkTitle')}
        desc={
          <span>
            {t('security.oidc.confirmUnlink')}
            {unlinkTarget?.providerName ? ` (${unlinkTarget.providerName})` : ''}
          </span>
        }
        cancelBtnText={t('common.cancel')}
        confirmText={isUnlinking ? t('common.loading') : t('common.unlink')}
        handleConfirm={handleUnlink}
        destructive
        isLoading={isUnlinking}
      />
    </div>
  );
}
