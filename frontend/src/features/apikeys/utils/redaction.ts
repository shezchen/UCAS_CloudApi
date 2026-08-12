/**
 * The server stores a redacted display form of every API key
 * (`ah-1a2b3c4d5...e5f6`) instead of the raw value, which it returns exactly
 * once when the key is created or rotated. Generated keys never contain a
 * dot, so the marker identifies a display value unambiguously — this mirrors
 * `xapikey.IsRedacted` on the server.
 */
const REDACTION_MARKER = '...';

export function isRedactedApiKey(key: string | null | undefined): boolean {
  return !!key && key.includes(REDACTION_MARKER);
}

/**
 * Placeholder used in usage examples when the raw key is not available, so a
 * copied snippet asks to be filled in rather than carrying a value that
 * cannot authenticate.
 */
export const API_KEY_PLACEHOLDER = 'YOUR_AXONHUB_API_KEY';
