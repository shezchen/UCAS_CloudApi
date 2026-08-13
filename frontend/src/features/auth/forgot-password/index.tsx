import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from '@/components/ui/card';
import AuthLayout from '../auth-layout';
import { ForgotPasswordForm } from './components/forgot-password-form';

export default function ForgotPassword() {
  const { t } = useTranslation();

  return (
    <AuthLayout>
      <div className='flex min-h-screen items-center justify-center px-4 py-20'>
        <Card className='w-full max-w-md gap-4'>
          <CardHeader>
            <CardTitle className='text-lg tracking-tight'>{t('auth.forgotPassword.title')}</CardTitle>
            <CardDescription>{t('auth.forgotPassword.description')}</CardDescription>
          </CardHeader>
          <CardContent>
            <ForgotPasswordForm />
          </CardContent>
          <CardFooter className='justify-center'>
            <Link to='/sign-in' className='text-muted-foreground hover:text-primary text-sm underline underline-offset-4'>
              {t('auth.forgotPassword.backToSignIn')}
            </Link>
          </CardFooter>
        </Card>
      </div>
    </AuthLayout>
  );
}
