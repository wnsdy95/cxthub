// Small profile avatar — displayed next to nickname in headers, etc.
// If photo(user.avatar) exists, use image; otherwise, use initials + color circle (same rules as profile page large avatar).
// If link is provided, clicking it moves to the user's profile (/{username}).
import type { User } from '../types';
import { navigate } from '../route';
import { useT } from '../i18n';
import { safeAvatarUrl } from '../urls';

// username (if none, use email/alias) hash generates a stable background color (fallback for profile page large avatar).
export function avatarColor(seed: string): string {
  let h = 0;
  for (let i = 0; i < seed.length; i++) h = (h * 31 + seed.charCodeAt(i)) >>> 0;
  return `hsl(${h % 360}, 42%, 52%)`;
}

type AvatarUser = Pick<User, 'avatar' | 'nickname' | 'name' | 'username' | 'email'>;

export function Avatar({ user, className, link }: { user: AvatarUser; className?: string; link?: boolean }) {
  const t = useT();
  const display = user.nickname || user.name || user.username || user.email || '?';
  const seed = user.username || user.email || display;
  const cls = className ? `avatar-sm ${className}` : 'avatar-sm';
  const avatar = safeAvatarUrl(user.avatar);
  const inner = avatar ? (
    <img className={`${cls} avatar-img`} src={avatar} alt={display} />
  ) : (
    <div className={cls} style={{ background: avatarColor(seed) }} aria-hidden="true">
      {display.trim().charAt(0).toUpperCase()}
    </div>
  );
  if (link && user.username) {
    return (
      <button
        className="avatar-link"
        onClick={() => navigate(`/${user.username}`)}
        aria-label={t('common.profileOf', { name: display })}
        title={t('common.profileOf', { name: display })}
      >
        {inner}
      </button>
    );
  }
  return inner;
}
