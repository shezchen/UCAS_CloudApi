import { AuthUser, getTokenFromStorage } from '@/stores/authStore';

// Same domain, no need to add baseURL.
export const API_BASE_URL = '';

type ErrorResponseBody = {
  message?: string;
  error?: string | { message?: string; code?: string };
};

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null;

const isErrorResponseBody = (value: unknown): value is ErrorResponseBody => {
  if (!isRecord(value)) {
    return false;
  }

  const message = value.message;
  const error = value.error;

  const hasValidMessage = message === undefined || typeof message === 'string';

  const hasValidError =
    error === undefined ||
    typeof error === 'string' ||
    (isRecord(error) &&
      (error.message === undefined || typeof error.message === 'string') &&
      (error.code === undefined || typeof error.code === 'string'));

  return hasValidMessage && hasValidError;
};

interface ApiRequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'DELETE' | 'PATCH';
  headers?: Record<string, string>;
  body?: any;
  requireAuth?: boolean;
}

export class ApiError extends Error {
  constructor(
    message: string,
    public status: number,
    public response?: unknown,
    public code?: string
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

export async function apiRequest<T>(endpoint: string, options: ApiRequestOptions = {}): Promise<T> {
  const { method = 'GET', headers = {}, body, requireAuth = false } = options;

  const url = `${API_BASE_URL}${endpoint}`;

  const requestHeaders: Record<string, string> = {
    'Content-Type': 'application/json',
    ...headers,
  };

  // Add Authorization header if auth is required
  if (requireAuth) {
    const token = getTokenFromStorage();
    if (token) {
      requestHeaders['Authorization'] = `Bearer ${token}`;
    }
  }

  const requestOptions: RequestInit = {
    method,
    headers: requestHeaders,
  };

  if (body && method !== 'GET') {
    requestOptions.body = JSON.stringify(body);
  }

  try {
    const response = await fetch(url, requestOptions);

    if (!response.ok) {
      let errorMessage = `HTTP ${response.status}: ${response.statusText}`;
      let errorData: unknown = null;
      let errorCode: string | undefined;

      try {
        errorData = await response.json();
        if (isErrorResponseBody(errorData)) {
          if (errorData.message) {
            errorMessage = errorData.message;
          } else if (errorData.error) {
            errorMessage = typeof errorData.error === 'string' ? errorData.error : errorData.error?.message || errorMessage;
          }
          if (typeof errorData.error === 'object' && errorData.error?.code) {
            errorCode = errorData.error.code;
          }
        }
      } catch {
        // If response is not JSON, use status text
      }

      throw new ApiError(errorMessage, response.status, errorData, errorCode);
    }

    // Handle empty responses
    const contentType = response.headers.get('content-type');
    if (contentType && contentType.includes('application/json')) {
      return (await response.json()) as T;
    }

    return {} as T;
  } catch (error) {
    if (error instanceof ApiError) {
      throw error;
    }

    const message = error instanceof Error ? error.message : 'Network error occurred';
    throw new ApiError(message, 0);
  }
}

// System API endpoints
export const systemApi = {
  getStatus: (): Promise<{ isInitialized: boolean }> => apiRequest('/admin/system/status'),

  initialize: (data: {
    ownerEmail: string;
    ownerPassword: string;
    ownerFirstName: string;
    ownerLastName: string;
    brandName: string;
    preferLanguage?: string;
  }): Promise<{ success: boolean; message: string }> =>
    apiRequest('/admin/system/initialize', {
      method: 'POST',
      body: data,
    }),
};

// Auth API endpoints
export const authApi = {
  signUp: (data: {
    email: string;
    password: string;
    nickname?: string;
    verificationCode: string;
  }): Promise<{
    user: AuthUser;
    token: string;
  }> =>
    apiRequest('/admin/auth/signup', {
      method: 'POST',
      body: data,
    }),

  sendSignUpVerification: (data: { email: string }): Promise<{ message?: string }> =>
    apiRequest('/admin/auth/signup/verification', {
      method: 'POST',
      body: data,
    }),

  requestPasswordResetVerification: (data: {
    email: string;
  }): Promise<{ message: string; challengeToken: string; resendAfterSeconds: number }> =>
    apiRequest('/admin/auth/password-reset/verification', {
      method: 'POST',
      body: data,
    }),

  resetPassword: (data: {
    email: string;
    challengeToken: string;
    verificationCode: string;
    newPassword: string;
  }): Promise<{ message: string }> =>
    apiRequest('/admin/auth/password-reset', {
      method: 'POST',
      body: data,
    }),

  signIn: (data: {
    email: string;
    password: string;
  }): Promise<{
    user: AuthUser;
    token: string;
  }> =>
    apiRequest('/admin/auth/signin', {
      method: 'POST',
      body: data,
    }),

  getOIDCProviders: (): Promise<{
    data: {
      id: string;
      name: string;
      display_name: string;
      jit_enabled: boolean;
      icon_url: string;
      button_color: string;
      active?: boolean;
      oidc_login_only: boolean;
      is_linked: boolean;
      linked_identity_id?: string;
      linked_email?: string;
    }[];
  }> => apiRequest('/oauth/oidc/providers', { requireAuth: true }),

  getOIDCAuthorizeURL: (provider: string): Promise<{ data: { url: string; state: string } }> =>
    apiRequest(`/oauth/oidc/authorize/${provider}`),

  getOIDCLinkAuthorizeURL: (provider: string): Promise<{ data: { url: string; state: string } }> =>
    apiRequest(`/admin/oidc/link/${provider}`, { requireAuth: true }),

  exchangeOIDCCode: (
    code: string
  ): Promise<{
    data: {
      user: AuthUser;
      token: string;
    };
  }> =>
    apiRequest('/oauth/oidc/exchange', {
      method: 'POST',
      body: { code },
    }),
};
