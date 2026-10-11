export function short(hash: string): string {
  return hash.replace(/^sha256:/, '').slice(0, 10);
}

export function when(iso?: string, compact = false): string {
  if (!iso || iso === '0001-01-01T00:00:00Z') return '';
  const date = new Date(iso);
  if (!Number.isFinite(date.getTime())) return '';
  const year = compact ? '' : `${date.getFullYear()}-`;
  return `${year}${String(date.getMonth() + 1).padStart(2, '0')}-${String(date.getDate()).padStart(2, '0')} ${String(date.getHours()).padStart(2, '0')}:${String(date.getMinutes()).padStart(2, '0')}`;
}
