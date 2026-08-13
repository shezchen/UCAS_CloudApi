import { type FormEvent, type HTMLAttributes, useEffect, useRef, useState } from 'react';
import { z } from 'zod';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useNavigate } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { useAuthStore } from '@/stores/authStore';
import { ApiError, authApi } from '@/lib/api-client';
import { cn } from '@/lib/utils';
import { passwordSchema } from '@/lib/validation';
import { Button } from '@/components/ui/button';
import { Form, FormControl, FormDescription, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form';
import { Input } from '@/components/ui/input';
import { PasswordInput } from '@/components/password-input';
import { resetSessionQueryCache } from '@/features/auth/data/auth';

type ForgotFormProps = HTMLAttributes<HTMLFormElement>;

type PasswordResetChallenge = {
  email: string;
  token: string;
};

export function ForgotPasswordForm({ className, ...props }: ForgotFormProps) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const resetSession = useAuthStore((state) => state.auth.reset);
  const [challenge, setChallenge] = useState<PasswordResetChallenge | null>(null);
  const [resendAvailableAt, setResendAvailableAt] = useState<number | null>(null);
  const [verificationCooldown, setVerificationCooldown] = useState(0);
  const operationInFlight = useRef(false);

  const formSchema = z
    .object({
      email: z
        .string()
        .trim()
        .email({ message: t('auth.forgotPassword.validation.invalidEmail') }),
      verificationCode: z
        .string()
        .trim()
        .regex(/^\d{6}$/, {
          message: t('auth.forgotPassword.validation.verificationCode'),
        }),
      newPassword: passwordSchema(t).refine((password) => Array.from(password).length >= 8, {
        message: t('auth.signIn.validation.passwordMinLength'),
      }),
      confirmPassword: z.string(),
    })
    .refine((data) => data.newPassword === data.confirmPassword, {
      message: t('auth.forgotPassword.validation.passwordMismatch'),
      path: ['confirmPassword'],
    });

  const form = useForm<z.infer<typeof formSchema>>({
    resolver: zodResolver(formSchema),
    defaultValues: {
      email: '',
      verificationCode: '',
      newPassword: '',
      confirmPassword: '',
    },
  });

  const errorMessage = (error: unknown, fallbackKey: string) => {
    if (error instanceof ApiError) {
      if (error.status === 429) {
        return t('auth.forgotPassword.error.rateLimited');
      }
      if (error.status === 503) {
        return t('auth.forgotPassword.error.emailUnavailable');
      }
      if (error.status === 0) {
        return t('auth.forgotPassword.error.network');
      }
      if (error.code === 'invalid_verification') {
        return t('auth.forgotPassword.error.invalidCode');
      }
      if (error.code === 'invalid_password') {
        return t('auth.forgotPassword.error.invalidPassword');
      }
      if (error.code === 'invalid_email') {
        return t('auth.forgotPassword.validation.invalidEmail');
      }

      return error.message || t(fallbackKey);
    }

    return error instanceof Error ? error.message : t(fallbackKey);
  };

  const requestVerification = useMutation({
    mutationFn: async (email: string) => {
      const response = await authApi.requestPasswordResetVerification({ email });
      if (!response.challengeToken) {
        throw new Error(t('auth.forgotPassword.error.invalidResponse'));
      }

      if (!Number.isFinite(response.resendAfterSeconds) || response.resendAfterSeconds < 0) {
        throw new Error(t('auth.forgotPassword.error.invalidResponse'));
      }

      return { email, token: response.challengeToken, resendAfterSeconds: response.resendAfterSeconds };
    },
    onSuccess: ({ email, token, resendAfterSeconds }) => {
      setChallenge({ email, token });
      setVerificationCooldown(Math.ceil(resendAfterSeconds));
      setResendAvailableAt(Date.now() + resendAfterSeconds * 1000);
      form.setValue('email', email);
      form.setValue('verificationCode', '');
      toast.success(t('auth.forgotPassword.verification.sent'));
    },
    onError: (error: unknown) => {
      toast.error(errorMessage(error, 'auth.forgotPassword.error.request'));
    },
    onSettled: () => {
      operationInFlight.current = false;
    },
  });

  const resetPassword = useMutation({
    mutationFn: authApi.resetPassword,
    onSuccess: () => {
      // The backend revokes every pre-reset browser JWT. Remove the matching
      // local session before entering the sign-in route, whose guard redirects
      // browsers that still appear authenticated.
      resetSession();
      resetSessionQueryCache(queryClient);
      toast.success(t('auth.forgotPassword.success'));
      navigate({ to: '/sign-in', replace: true });
    },
    onError: (error: unknown) => {
      toast.error(errorMessage(error, 'auth.forgotPassword.error.reset'));
    },
    onSettled: () => {
      operationInFlight.current = false;
    },
  });

  useEffect(() => {
    if (resendAvailableAt === null) {
      setVerificationCooldown(0);
      return;
    }

    const updateCooldown = () => {
      const seconds = Math.max(0, Math.ceil((resendAvailableAt - Date.now()) / 1000));
      setVerificationCooldown(seconds);
      if (seconds === 0) {
        setResendAvailableAt(null);
      }
    };
    updateCooldown();
    const timer = window.setInterval(updateCooldown, 250);

    return () => window.clearInterval(timer);
  }, [resendAvailableAt]);

  const isBusy = requestVerification.isPending || resetPassword.isPending;

  async function onSendVerification() {
    if (operationInFlight.current || isBusy) {
      return;
    }
    operationInFlight.current = true;

    const emailIsValid = await form.trigger('email', { shouldFocus: true });
    if (!emailIsValid) {
      operationInFlight.current = false;
      return;
    }

    requestVerification.mutate(form.getValues('email').trim().toLowerCase());
  }

  function onResetPassword(data: z.infer<typeof formSchema>) {
    if (!challenge) {
      operationInFlight.current = false;
      return;
    }

    resetPassword.mutate({
      email: challenge.email,
      challengeToken: challenge.token,
      verificationCode: data.verificationCode,
      newPassword: data.newPassword,
    });
  }

  function onChangeEmail() {
    if (operationInFlight.current || isBusy) {
      return;
    }

    const email = challenge?.email ?? form.getValues('email');
    setChallenge(null);
    setResendAvailableAt(null);
    setVerificationCooldown(0);
    form.reset({
      email,
      verificationCode: '',
      newPassword: '',
      confirmPassword: '',
    });
  }

  function onFormSubmit(event: FormEvent<HTMLFormElement>) {
    if (!challenge) {
      event.preventDefault();
      void onSendVerification();
      return;
    }

    if (operationInFlight.current || isBusy) {
      event.preventDefault();
      return;
    }
    operationInFlight.current = true;
    void form.handleSubmit(onResetPassword, () => {
      operationInFlight.current = false;
    })(event);
  }

  return (
    <Form {...form}>
      <form {...props} onSubmit={onFormSubmit} className={cn('grid gap-3', className)}>
        <FormField
          control={form.control}
          name='email'
          render={({ field }) => (
            <FormItem>
              <div className='flex items-center justify-between gap-3'>
                <FormLabel>{t('auth.forgotPassword.email')}</FormLabel>
                {challenge && (
                  <Button
                    type='button'
                    variant='link'
                    className='h-auto p-0 text-xs'
                    data-testid='forgot-password-change-email'
                    disabled={isBusy}
                    onClick={onChangeEmail}
                  >
                    {t('auth.forgotPassword.changeEmail')}
                  </Button>
                )}
              </div>
              <FormControl>
                <Input
                  type='email'
                  autoComplete='email'
                  placeholder='name@example.com'
                  readOnly={Boolean(challenge) || isBusy}
                  data-testid='forgot-password-email'
                  className={challenge ? 'bg-muted' : undefined}
                  {...field}
                />
              </FormControl>
              {!challenge && <FormDescription>{t('auth.forgotPassword.emailDescription')}</FormDescription>}
              <FormMessage />
            </FormItem>
          )}
        />

        {challenge && (
          <>
            <FormField
              control={form.control}
              name='verificationCode'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('auth.forgotPassword.verificationCode')}</FormLabel>
                  <div className='flex items-start gap-2'>
                    <FormControl>
                      <Input
                        autoComplete='one-time-code'
                        inputMode='numeric'
                        placeholder='000000'
                        data-testid='forgot-password-code'
                        disabled={isBusy}
                        {...field}
                        onChange={(event) => field.onChange(event.target.value.replace(/\D/g, '').slice(0, 6))}
                      />
                    </FormControl>
                    <Button
                      type='button'
                      variant='outline'
                      className='shrink-0'
                      data-testid='forgot-password-resend'
                      disabled={isBusy || verificationCooldown > 0}
                      onClick={onSendVerification}
                    >
                      {requestVerification.isPending
                        ? t('auth.forgotPassword.verification.sending')
                        : verificationCooldown > 0
                          ? t('auth.forgotPassword.verification.cooldown', { seconds: verificationCooldown })
                          : t('auth.forgotPassword.verification.resend')}
                    </Button>
                  </div>
                  <FormDescription>{t('auth.forgotPassword.verification.description')}</FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />

            <FormField
              control={form.control}
              name='newPassword'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('auth.forgotPassword.newPassword')}</FormLabel>
                  <FormControl>
                    <PasswordInput
                      autoComplete='new-password'
                      placeholder='********'
                      data-testid='forgot-password-new-password'
                      disabled={isBusy}
                      {...field}
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />

            <FormField
              control={form.control}
              name='confirmPassword'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('auth.forgotPassword.confirmPassword')}</FormLabel>
                  <FormControl>
                    <PasswordInput
                      autoComplete='new-password'
                      placeholder='********'
                      data-testid='forgot-password-confirm-password'
                      disabled={isBusy}
                      {...field}
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
          </>
        )}

        <Button className='mt-2' disabled={isBusy} data-testid='forgot-password-submit'>
          {challenge
            ? resetPassword.isPending
              ? t('auth.forgotPassword.resetting')
              : t('auth.forgotPassword.reset')
            : requestVerification.isPending
              ? t('auth.forgotPassword.verification.sending')
              : t('auth.forgotPassword.continue')}
        </Button>
      </form>
    </Form>
  );
}
