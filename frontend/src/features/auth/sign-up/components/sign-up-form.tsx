import { HTMLAttributes } from 'react';
import { z } from 'zod';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { cn } from '@/lib/utils';
import { passwordSchema } from '@/lib/validation';
import { Button } from '@/components/ui/button';
import { Form, FormControl, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form';
import { Input } from '@/components/ui/input';
import { PasswordInput } from '@/components/password-input';
import { useSignUp } from '@/features/auth/data/auth';

type SignUpFormProps = HTMLAttributes<HTMLFormElement>;

import { useTranslation } from 'react-i18next';

const campusEmailDomains = new Set(['mails.ucas.ac.cn', 'ucas.ac.cn', 'mails.ucas.edu.cn', 'ucas.edu.cn']);

export function SignUpForm({ className, ...props }: SignUpFormProps) {
  const { t } = useTranslation();
  const signUp = useSignUp();

  const formSchema = z
    .object({
      email: z
        .email({ message: t('auth.signUp.validation.invalidEmail') })
        .refine((email) => campusEmailDomains.has(email.trim().toLowerCase().split('@').at(-1) || ''), {
          message: t('auth.signUp.validation.campusEmail'),
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
      password: '',
      confirmPassword: '',
    },
  });

  function onSubmit(data: z.infer<typeof formSchema>) {
    signUp.mutate({ email: data.email, password: data.password });
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
