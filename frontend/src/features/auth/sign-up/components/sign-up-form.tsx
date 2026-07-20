import { type HTMLAttributes, useEffect, useState } from 'react';
import { z } from 'zod';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { useMutation } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { cn } from '@/lib/utils';
import { authApi } from '@/lib/api-client';
import { passwordSchema } from '@/lib/validation';
import { Button } from '@/components/ui/button';
import { Form, FormControl, FormDescription, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form';
import { Input } from '@/components/ui/input';
import { PasswordInput } from '@/components/password-input';
import { useSignUp } from '@/features/auth/data/auth';

type SignUpFormProps = HTMLAttributes<HTMLFormElement>;

const campusEmailDomains = new Set(['mails.ucas.ac.cn', 'ucas.ac.cn', 'mails.ucas.edu.cn', 'ucas.edu.cn']);

export function SignUpForm({ className, ...props }: SignUpFormProps) {
  const { t } = useTranslation();
  const signUp = useSignUp();
  const [verificationCooldown, setVerificationCooldown] = useState(0);

  const formSchema = z
    .object({
      email: z
        .email({ message: t('auth.signUp.validation.invalidEmail') })
        .refine((email) => campusEmailDomains.has(email.trim().toLowerCase().split('@').at(-1) || ''), {
          message: t('auth.signUp.validation.campusEmail'),
        }),
      nickname: z
        .string()
        .trim()
        .refine(
          (nickname) => {
            const length = Array.from(nickname).length;
            return length === 0 || (length >= 2 && length <= 24);
          },
          {
            message: t('auth.signUp.validation.nicknameLength'),
          }
        ),
      verificationCode: z.string().trim().regex(/^\d{6}$/, {
        message: t('auth.signUp.validation.verificationCode'),
      }),
      password: passwordSchema(t),
      confirmPassword: z.string(),
    })
    .refine((data) => data.password === data.confirmPassword, {
      message: t('auth.signUp.validation.passwordMismatch'),
      path: ['confirmPassword'],
    });

  const form = useForm<z.infer<typeof formSchema>>({
    resolver: zodResolver(formSchema),
    defaultValues: {
      email: '',
      nickname: '',
      verificationCode: '',
      password: '',
      confirmPassword: '',
    },
  });

  const sendVerification = useMutation({
    mutationFn: (email: string) => authApi.sendSignUpVerification({ email }),
    onSuccess: () => {
      setVerificationCooldown(60);
      toast.success(t('auth.signUp.verification.sent'));
    },
    onError: (error: unknown) => {
      toast.error(error instanceof Error ? error.message : t('auth.signUp.verification.error'));
    },
  });

  useEffect(() => {
    if (verificationCooldown <= 0) {
      return;
    }

    const timer = window.setTimeout(() => {
      setVerificationCooldown((seconds) => Math.max(0, seconds - 1));
    }, 1000);

    return () => window.clearTimeout(timer);
  }, [verificationCooldown]);

  async function onSendVerification() {
    const emailIsValid = await form.trigger('email', { shouldFocus: true });
    if (!emailIsValid) {
      return;
    }

    sendVerification.mutate(form.getValues('email').trim().toLowerCase());
  }

  function onSubmit(data: z.infer<typeof formSchema>) {
    signUp.mutate({
      email: data.email.trim().toLowerCase(),
      password: data.password,
      nickname: data.nickname || undefined,
      verificationCode: data.verificationCode,
    });
  }

  return (
    <Form {...form}>
      <form onSubmit={form.handleSubmit(onSubmit)} className={cn('grid gap-3', className)} {...props}>
        <FormField
          control={form.control}
          name='email'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('auth.signUp.email')}</FormLabel>
              <FormControl>
                <Input type='email' placeholder='name@mails.ucas.ac.cn' {...field} />
              </FormControl>
              <FormMessage />
            </FormItem>
          )}
        />
        <FormField
          control={form.control}
          name='nickname'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('auth.signUp.nickname')}</FormLabel>
              <FormControl>
                <Input autoComplete='nickname' placeholder={t('auth.signUp.nicknamePlaceholder')} {...field} />
              </FormControl>
              <FormDescription>{t('auth.signUp.nicknameDescription')}</FormDescription>
              <FormMessage />
            </FormItem>
          )}
        />
        <FormField
          control={form.control}
          name='verificationCode'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('auth.signUp.verificationCode')}</FormLabel>
              <div className='flex items-start gap-2'>
                <FormControl>
                  <Input
                    autoComplete='one-time-code'
                    inputMode='numeric'
                    maxLength={6}
                    placeholder='000000'
                    {...field}
                    onChange={(event) => field.onChange(event.target.value.replace(/\D/g, '').slice(0, 6))}
                  />
                </FormControl>
                <Button
                  type='button'
                  variant='outline'
                  className='shrink-0'
                  disabled={sendVerification.isPending || verificationCooldown > 0}
                  onClick={onSendVerification}
                >
                  {sendVerification.isPending
                    ? t('auth.signUp.verification.sending')
                    : verificationCooldown > 0
                      ? t('auth.signUp.verification.cooldown', { seconds: verificationCooldown })
                      : t('auth.signUp.verification.send')}
                </Button>
              </div>
              <FormDescription>{t('auth.signUp.verification.description')}</FormDescription>
              <FormMessage />
            </FormItem>
          )}
        />
        <FormField
          control={form.control}
          name='password'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('auth.signUp.password')}</FormLabel>
              <FormControl>
                <PasswordInput placeholder='********' {...field} />
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
              <FormLabel>{t('users.form.confirmPassword')}</FormLabel>
              <FormControl>
                <PasswordInput placeholder='********' {...field} />
              </FormControl>
              <FormMessage />
            </FormItem>
          )}
        />
        <Button className='mt-2' disabled={signUp.isPending}>
          {signUp.isPending ? t('auth.signUp.submitting') : t('auth.signUp.submit')}
        </Button>

        {/* <div className='relative my-2'>
          <div className='absolute inset-0 flex items-center'>
            <span className='w-full border-t' />
          </div>
          <div className='relative flex justify-center text-xs uppercase'>
            <span className='bg-background text-muted-foreground px-2'>
              Or continue with
            </span>
          </div>
        </div>

        <div className='grid grid-cols-2 gap-2'>
          <Button
            variant='outline'
            className='w-full'
            type='button'
            disabled={isLoading}
          >
            <IconBrandGithub className='h-4 w-4' /> GitHub
          </Button>
          <Button
            variant='outline'
            className='w-full'
            type='button'
            disabled={isLoading}
          >
            <IconBrandFacebook className='h-4 w-4' /> Facebook
          </Button>
        </div> */}
      </form>
    </Form>
  );
}
