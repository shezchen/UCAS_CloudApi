import { useEffect, useState, type ComponentProps } from 'react';
import { cn, extractNumberID } from '@/lib/utils';
import { apiRequestBlob } from '@/lib/api-client';
import { useSelectedProjectId } from '@/stores/projectStore';
import { Avatar, AvatarFallback, AvatarImage } from '@/components/ui/avatar';

type UserAvatarProps = Omit<ComponentProps<typeof Avatar>, 'children'> & {
  avatar?: string | null;
  userId?: string | null;
  label?: string | null;
  alt?: string;
};

/** Session-scoped object URL cache so list rows share one fetch per avatar source. */
const avatarObjectUrlCache = new Map<string, Promise<string>>();

function getAvatarFallback(label?: string | null): string {
  const normalized = label?.trim();
  if (!normalized) return 'U';

  const identity = normalized.includes('@') ? normalized.split('@', 1)[0] : normalized;
  const words = identity.split(/\s+/).filter(Boolean);
  if (words.length > 1) {
    return `${Array.from(words[0])[0] ?? ''}${Array.from(words.at(-1) ?? '')[0] ?? ''}`.toUpperCase();
  }

  return Array.from(identity).slice(0, 2).join('').toUpperCase();
}

function resolveAvatarSource(avatar?: string | null, userId?: string | null): string | undefined {
  const trimmed = avatar?.trim();
  if (trimmed) return trimmed;
  if (!userId) return undefined;
  const numericID = extractNumberID(userId);
  if (!numericID) return undefined;
  return `/admin/users/${numericID}/avatar`;
}

function isDirectAvatarSource(source: string): boolean {
  return source.startsWith('http://') || source.startsWith('https://') || source.startsWith('data:');
}

function isAdminAvatarPath(source: string): boolean {
  return source.startsWith('/admin/');
}

function isCampusAvatarPath(source: string): boolean {
  return source.startsWith('/admin/avatars/campus/');
}

function avatarCacheKey(source: string, projectId: string | null): string {
  // Campus aliases collide across projects; user avatar bytes do not.
  if (isCampusAvatarPath(source)) {
    return `${projectId ?? ''}::${source}`;
  }
  return source;
}

function loadAdminAvatar(source: string, projectId: string | null): Promise<string> {
  const cacheKey = avatarCacheKey(source, projectId);
  const cached = avatarObjectUrlCache.get(cacheKey);
  if (cached) return cached;

  const headers: Record<string, string> = {};
  if (projectId) {
    headers['X-Project-ID'] = projectId;
  }

  const pending = apiRequestBlob(source, {
    requireAuth: true,
    headers,
  })
    .then((blob) => URL.createObjectURL(blob))
    .catch((error) => {
      avatarObjectUrlCache.delete(cacheKey);
      throw error;
    });

  avatarObjectUrlCache.set(cacheKey, pending);
  return pending;
}

export function UserAvatar({ avatar, userId, label, alt, className, ...props }: UserAvatarProps) {
  const projectId = useSelectedProjectId();
  const source = resolveAvatarSource(avatar, userId);
  const [resolvedSrc, setResolvedSrc] = useState<string | undefined>(() =>
    source && isDirectAvatarSource(source) ? source : undefined
  );

  useEffect(() => {
    let cancelled = false;

    if (!source) {
      setResolvedSrc(undefined);
      return;
    }

    if (isDirectAvatarSource(source)) {
      setResolvedSrc(source);
      return;
    }

    if (!isAdminAvatarPath(source)) {
      setResolvedSrc(undefined);
      return;
    }

    // Campus aliases are project-scoped; user avatars may be fetched without a
    // project header (system /users for owners).
    if (isCampusAvatarPath(source) && !projectId) {
      setResolvedSrc(undefined);
      return;
    }

    setResolvedSrc(undefined);
    void loadAdminAvatar(source, projectId)
      .then((objectUrl) => {
        if (!cancelled) setResolvedSrc(objectUrl);
      })
      .catch(() => {
        if (!cancelled) setResolvedSrc(undefined);
      });

    return () => {
      cancelled = true;
      // Shared cache owns object URLs for the session; do not revoke on unmount.
    };
  }, [projectId, source]);

  return (
    <Avatar className={cn('size-8 shrink-0', className)} {...props}>
      {resolvedSrc && <AvatarImage src={resolvedSrc} alt={alt ?? label ?? ''} className='object-cover' />}
      <AvatarFallback>{getAvatarFallback(label)}</AvatarFallback>
    </Avatar>
  );
}
