export function sanitizeRemoteUrl(raw?: string): string {
  const value = raw?.trim() ?? '';
  if (!value) return '';
  try {
    const parsed = new URL(value);
    if (!['http:', 'https:', 'ssh:', 'git:', 'git+ssh:'].includes(parsed.protocol) || !parsed.host) return '';
    parsed.username = '';
    parsed.password = '';
    parsed.search = '';
    parsed.hash = '';
    return parsed.toString();
  } catch {
    const at = value.lastIndexOf('@');
    const rest = at >= 0 ? value.slice(at + 1) : '';
    const colon = rest.indexOf(':');
    if (colon > 0 && colon < rest.length - 1 && !/[\\/\s]/.test(rest.slice(0, colon)) && !/[\u0000-\u0020\u007f]/.test(rest)) {
      return `git@${rest}`;
    }
    return '';
  }
}

export function safeHttpUrl(raw?: string): string | null {
  const value = raw?.trim() ?? '';
  if (!value) return null;
  try {
    const parsed = new URL(value);
    if (!['http:', 'https:'].includes(parsed.protocol) || !parsed.hostname || parsed.username || parsed.password) return null;
    return parsed.toString();
  } catch {
    return null;
  }
}

export function safeAvatarUrl(raw?: string): string {
  const value = raw?.trim() ?? '';
  if (!value || value.length > 700_000) return '';
  return /^data:image\/(?:jpeg|jpg|png|webp|gif);base64,[A-Za-z0-9+/]+={0,2}$/.test(value) ? value : '';
}

export function gitWebUrl(raw?: string): string | null {
  let value = sanitizeRemoteUrl(raw);
  if (!value) return null;
  if (value.startsWith('git@')) value = `https://${value.slice(4).replace(':', '/')}`;
  try {
    const parsed = new URL(value);
    if ((parsed.protocol !== 'http:' && parsed.protocol !== 'https:') || !parsed.hostname) return null;
    parsed.username = '';
    parsed.password = '';
    parsed.search = '';
    parsed.hash = '';
    return parsed.toString().replace(/\.git\/?$/, '').replace(/\/$/, '');
  } catch {
    return null;
  }
}
