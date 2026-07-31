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

export function UserAvatar({ avatar, userId, label, alt, className, ...props }: UserAvatarProps) {
  const projectId = useSelectedProjectId();
  const source = resolveAvatarSource(avatar, userId);
  const [resolvedSrc, setResolvedSrc] = useState<string | undefined>(() =>
    source && isDirectAvatarSource(source) ? source : undefined
  );

  useEffect(() => {
    let cancelled = false;
    let objectUrl: string | undefined;

    if (!source) {
      setResolvedSrc(undefined);
      return;
    }

    if (isDirectAvatarSource(source)) {
      setResolvedSrc(source);
      return;
    }

    if (!isAdminAvatarPath(source) || !projectId) {
      setResolvedSrc(undefined);
      return;
    }

    setResolvedSrc(undefined);
    void apiRequestBlob(source, {
      requireAuth: true,
      headers: { 'X-Project-ID': projectId },
    })
      .then((blob) => {
        if (cancelled) return;
        objectUrl = URL.createObjectURL(blob);
        setResolvedSrc(objectUrl);
      })
      .catch(() => {
        if (!cancelled) setResolvedSrc(undefined);
      });

    return () => {
      cancelled = true;
      if (objectUrl) URL.revokeObjectURL(objectUrl);
    };
  }, [projectId, source]);

  return (
    <Avatar className={cn('size-8 shrink-0', className)} {...props}>
      {resolvedSrc && <AvatarImage src={resolvedSrc} alt={alt ?? label ?? ''} className='object-cover' />}
      <AvatarFallback>{getAvatarFallback(label)}</AvatarFallback>
    </Avatar>
  );
}
