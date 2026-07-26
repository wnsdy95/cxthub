// Markdown — dependency-free mini-markdown renderer (React elements — no raw HTML, no XSS). For displaying markdown-like data such as compressed memory digest·agent summary.
// Supported: #/##/### headings · -/* lists · "1." ordered lists · ``` code blocks · inline `code`/**bold**/**/ · [links](http…). Others as paragraphs.
import React from 'react';
import { safeHttpUrl } from '../urls';

const INLINE_RE = /(\*\*[^*]+\*\*|`[^`]+`|\[[^\]]+\]\(https?:\/\/[^)\s]+\))/g;

function inline(text: string): React.ReactNode[] {
  const parts: React.ReactNode[] = [];
  let last = 0;
  let i = 0;
  for (const m of text.matchAll(INLINE_RE)) {
    const idx = m.index ?? 0;
    if (idx > last) parts.push(text.slice(last, idx));
    const tok = m[0];
    if (tok.startsWith('**')) {
      parts.push(<strong key={i++}>{tok.slice(2, -2)}</strong>);
    } else if (tok.startsWith('`')) {
      parts.push(<code key={i++}>{tok.slice(1, -1)}</code>);
    } else {
      const mm = tok.match(/^\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)$/);
      if (mm) {
        const href = safeHttpUrl(mm[2]);
        parts.push(href ? (
          <a key={i++} href={href} target="_blank" rel="noopener noreferrer">
            {mm[1]}
          </a>
        ) : mm[1]);
      } else {
        parts.push(tok);
      }
    }
    last = idx + tok.length;
  }
  if (last < text.length) parts.push(text.slice(last));
  return parts;
}

export function Markdown({ text }: { text: string }) {
  const out: React.ReactNode[] = [];
  const lines = text.split('\n');
  let i = 0;
  let key = 0;
  while (i < lines.length) {
    const line = lines[i];
    // Code block
    if (line.trimStart().startsWith('```')) {
      const buf: string[] = [];
      i++;
      while (i < lines.length && !lines[i].trimStart().startsWith('```')) {
        buf.push(lines[i]);
        i++;
      }
      i++; // closing fence
      out.push(<pre key={key++}>{buf.join('\n')}</pre>);
      continue;
    }
    // Heading
    const h = line.match(/^(#{1,3})\s+(.*)$/);
    if (h) {
      const Tag = (['h4', 'h5', 'h6'] as const)[h[1].length - 1]; // nested panel, so small steps
      out.push(<Tag key={key++}>{inline(h[2])}</Tag>);
      i++;
      continue;
    }
    // List (unordered/ordered) — consecutive items into one list
    const isUl = (l: string) => /^\s*[-*]\s+/.test(l);
    const isOl = (l: string) => /^\s*\d+[.)]\s+/.test(l);
    if (isUl(line) || isOl(line)) {
      const ordered = isOl(line);
      const items: React.ReactNode[] = [];
      while (i < lines.length && (ordered ? isOl(lines[i]) : isUl(lines[i]))) {
        items.push(<li key={key++}>{inline(lines[i].replace(ordered ? /^\s*\d+[.)]\s+/ : /^\s*[-*]\s+/, ''))}</li>);
        i++;
      }
      out.push(ordered ? <ol key={key++}>{items}</ol> : <ul key={key++}>{items}</ul>);
      continue;
    }
    // Empty line → Paragraph boundary
    if (line.trim() === '') {
      i++;
      continue;
    }
    // Paragraph: Concatenates until the next block element or empty line
    const buf: string[] = [];
    while (i < lines.length && lines[i].trim() !== '' && !/^(#{1,3})\s/.test(lines[i]) && !isUl(lines[i]) && !isOl(lines[i]) && !lines[i].trimStart().startsWith('```')) {
      buf.push(lines[i]);
      i++;
    }
    out.push(<p key={key++}>{inline(buf.join('\n'))}</p>);
  }
  return <div className="md">{out}</div>;
}
