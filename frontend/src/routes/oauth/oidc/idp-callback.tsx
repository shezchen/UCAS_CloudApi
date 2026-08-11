import { useEffect, useRef } from 'react';
import { createFileRoute, useNavigate } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { useOIDCExchange } from '@/features/auth/data/auth';
import { Loader2 } from 'lucide-react';
import { toast } from 'sonner';

export const Route = createFileRoute('/oauth/oidc/idp-callback')({
  component: OIDCCallback,
  validateSearch: (search: Record<string, unknown>) => {
    return {
      code: (search.code as string) || '',
      error: search.error as string | undefined,
      error_description: search.error_description as string | undefined,
    };
  },
});

function OIDCCallback() {
  const { t } = useTranslation();
  const { code, error, error_description } = Route.useSearch();
  const navigate = useNavigate();
  const exchangeMutation = useOIDCExchange();
  const hasAttemptedRef = useRef(false);

  useEffect(() => {
    // Prevent strict mode double-firing
    if (hasAttemptedRef.current) return;
    hasAttemptedRef.current = true;

    if (error) {
      console.error('OIDC Error:', error, error_description);
      toast.error(error_description || error || t('auth.oidcCallback.authFailed'));
      navigate({ to: '/sign-in' });
      return;
    }

    if (!code) {
      toast.error(t('auth.oidcCallback.missingCode'));
      navigate({ to: '/sign-in' });
      return;
    }

    exchangeMutation.mutate(code);
  }, [code, error, error_description, navigate, exchangeMutation, t]);

  return (
    <div className="flex h-screen w-full flex-col items-center justify-center bg-slate-50">
      <div className="flex flex-col items-center space-y-4 rounded-xl bg-white p-8 text-center shadow-lg">
        <div className="flex h-16 w-16 items-center justify-center rounded-full bg-slate-100">
          <Loader2 className="h-8 w-8 animate-spin text-slate-800" />
        </div>
        <div className="space-y-2">
          <h2 className="text-xl font-semibold text-slate-900">{t('auth.oidcCallback.title')}</h2>
          <p className="text-sm text-slate-500">{t('auth.oidcCallback.description')}</p>
        </div>
      </div>
    </div>
  );
}
