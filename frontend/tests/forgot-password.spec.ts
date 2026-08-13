import { expect, test } from '@playwright/test'

test.describe('Password reset', () => {
  test.beforeEach(async ({ page }) => {
    await page.route('**/admin/system/status', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ isInitialized: true }),
      })
    })
  })

  test('requests a code, resets the password, and clears a stale browser session', async ({ page }) => {
    let verificationRequest: unknown
    let resetRequest: unknown
    let verificationRequests = 0
    let resetRequests = 0

    await page.route('**/admin/auth/password-reset/verification', async (route) => {
      verificationRequests += 1
      verificationRequest = route.request().postDataJSON()
      await route.fulfill({
        status: 202,
        contentType: 'application/json',
        body: JSON.stringify({
          message: 'If the account exists, a password reset code has been sent.',
          challengeToken: 'v1.17.4102444800.test-signature',
          resendAfterSeconds: 60,
        }),
      })
    })
    await page.route('**/admin/auth/password-reset', async (route) => {
      resetRequests += 1
      resetRequest = route.request().postDataJSON()
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ message: 'Password reset successfully.' }),
      })
    })

    await page.goto('/forgot-password')
    await page.getByTestId('forgot-password-email').fill(' Owner@Example.com ')
    await page.getByTestId('forgot-password-submit').evaluate((button: HTMLButtonElement) => {
      button.click()
      button.click()
    })

    await expect(page.getByTestId('forgot-password-code')).toBeVisible()
    expect(verificationRequests).toBe(1)
    expect(verificationRequest).toEqual({ email: 'owner@example.com' })

    await page.getByTestId('forgot-password-code').fill('12x3456')
    await expect(page.getByTestId('forgot-password-code')).toHaveValue('123456')
    await page.getByTestId('forgot-password-new-password').fill('new-password-123')
    await page.getByTestId('forgot-password-confirm-password').fill('new-password-123')

    await page.evaluate(() => {
      localStorage.setItem('axonhub_access_token', 'stale-token')
      localStorage.setItem('axonhub_user_info', '{"id":"stale-user"}')
    })
    await page.getByTestId('forgot-password-submit').evaluate((button: HTMLButtonElement) => {
      button.click()
      button.click()
    })

    await expect(page).toHaveURL(/\/sign-in$/)
    expect(resetRequests).toBe(1)
    expect(resetRequest).toEqual({
      email: 'owner@example.com',
      challengeToken: 'v1.17.4102444800.test-signature',
      verificationCode: '123456',
      newPassword: 'new-password-123',
    })
    await expect
      .poll(() =>
        page.evaluate(() => ({
          token: localStorage.getItem('axonhub_access_token'),
          user: localStorage.getItem('axonhub_user_info'),
        }))
      )
      .toEqual({ token: null, user: null })
  })

  test('keeps the form usable after an invalid or expired code', async ({ page }) => {
    await page.route('**/admin/auth/password-reset/verification', async (route) => {
      await route.fulfill({
        status: 202,
        contentType: 'application/json',
        body: JSON.stringify({
          message: 'Accepted',
          challengeToken: 'v1.18.4102444800.test-signature',
          resendAfterSeconds: 60,
        }),
      })
    })
    await page.route('**/admin/auth/password-reset', async (route) => {
      await route.fulfill({
        status: 400,
        contentType: 'application/json',
        body: JSON.stringify({
          error: { message: 'Email verification code is invalid or expired', code: 'invalid_verification' },
        }),
      })
    })

    await page.goto('/forgot-password')
    await page.getByTestId('forgot-password-email').fill('student@mails.ucas.ac.cn')
    await page.getByTestId('forgot-password-submit').click()
    await page.getByTestId('forgot-password-code').fill('123456')
    await page.getByTestId('forgot-password-new-password').fill('😀😀😀😀')
    await page.getByTestId('forgot-password-confirm-password').fill('😀😀😀😀')
    await page.getByTestId('forgot-password-submit').click()
    await expect(page.getByText(/at least 8 characters|至少需要 8/i)).toBeVisible()

    await page.getByTestId('forgot-password-new-password').fill('new-password-123')
    await page.getByTestId('forgot-password-confirm-password').fill('new-password-123')
    await page.getByTestId('forgot-password-submit').click()

    await expect(page.getByText(/invalid, expired, or already used|无效、已过期或已被使用/i)).toBeVisible()
    await expect(page.getByTestId('forgot-password-code')).toHaveValue('123456')
    await expect(page.getByTestId('forgot-password-submit')).toBeEnabled()
  })
})
