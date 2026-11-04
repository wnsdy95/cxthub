export type MarkdownInline =
  | { type: 'text'; text: string }
  | { type: 'strong'; text: string }
  | { type: 'code'; text: string }
  | { type: 'link'; text: string; href: string };

export type MarkdownBlock =
  | { type: 'paragraph'; children: MarkdownInline[] }
  | { type: 'pre'; text: string }
  | { type: 'heading'; level: 1 | 2 | 3; children: MarkdownInline[] }
  | { type: 'list'; ordered: boolean; items: MarkdownInline[][] };

export type MarkdownPresentation = MarkdownBlock[];

type BlockStart =
  | { type: 'pre' }
  | { type: 'heading'; level: 1 | 2 | 3; text: string }
  | { type: 'list'; ordered: boolean };

const URL_TERMINATOR_RE = /[)\s]/;
const HEADING_RE = /^(#{1,3})\s+(.*)$/;
const UNORDERED_LIST_RE = /^\s*[-*]\s+/;
const ORDERED_LIST_RE = /^\s*\d+[.)]\s+/;

function parseInline(text: string): MarkdownInline[] {
  const parts: MarkdownInline[] = [];
  let lastIndex = 0;
  let cursor = 0;
  let nextStar = -1;
  let nextBacktick = -1;
  let nextBracket = -1;
  let nextUrlTerminator = -1;
  while (cursor < text.length) {
    const marker = text[cursor];
    let token: MarkdownInline | undefined;
    let tokenEnd = cursor;
    if (marker === '*' && text[cursor + 1] === '*') {
      const contentStart = cursor + 2;
      if (nextStar < contentStart) {
        nextStar = text.indexOf('*', contentStart);
        if (nextStar < 0) nextStar = text.length;
      }
      if (nextStar > contentStart && text[nextStar + 1] === '*') {
        token = { type: 'strong', text: text.slice(contentStart, nextStar) };
        tokenEnd = nextStar + 2;
      }
    } else if (marker === '`') {
      const contentStart = cursor + 1;
      if (nextBacktick < contentStart) {
        nextBacktick = text.indexOf('`', contentStart);
        if (nextBacktick < 0) nextBacktick = text.length;
      }
      if (nextBacktick > contentStart && nextBacktick < text.length) {
        token = { type: 'code', text: text.slice(contentStart, nextBacktick) };
        tokenEnd = nextBacktick + 1;
      }
    } else if (marker === '[') {
      const labelStart = cursor + 1;
      if (nextBracket < labelStart) {
        nextBracket = text.indexOf(']', labelStart);
        if (nextBracket < 0) nextBracket = text.length;
      }
      if (nextBracket > labelStart && text[nextBracket + 1] === '(') {
        const hrefStart = nextBracket + 2;
        const contentStart = text.startsWith('https://', hrefStart) ? hrefStart + 8
          : text.startsWith('http://', hrefStart) ? hrefStart + 7 : -1;
        if (contentStart >= 0) {
          if (nextUrlTerminator < contentStart) {
            nextUrlTerminator = contentStart;
            while (nextUrlTerminator < text.length && !URL_TERMINATOR_RE.test(text[nextUrlTerminator])) nextUrlTerminator++;
          }
          if (nextUrlTerminator > contentStart && text[nextUrlTerminator] === ')') {
            token = { type: 'link', text: text.slice(labelStart, nextBracket), href: text.slice(hrefStart, nextUrlTerminator) };
            tokenEnd = nextUrlTerminator + 1;
          }
        }
      }
    }
    if (token) {
      if (cursor > lastIndex) parts.push({ type: 'text', text: text.slice(lastIndex, cursor) });
      parts.push(token);
      cursor = tokenEnd;
      lastIndex = tokenEnd;
    } else {
      cursor++;
    }
  }
  if (lastIndex < text.length) parts.push({ type: 'text', text: text.slice(lastIndex) });
  return parts;
}

function recognizeBlock(line: string): BlockStart | undefined {
  if (line.trimStart().startsWith('```')) return { type: 'pre' };
  const heading = line.match(HEADING_RE);
  if (heading) return { type: 'heading', level: heading[1].length as 1 | 2 | 3, text: heading[2] };
  if (UNORDERED_LIST_RE.test(line)) return { type: 'list', ordered: false };
  if (ORDERED_LIST_RE.test(line)) return { type: 'list', ordered: true };
  return undefined;
}

export function parseMarkdownPresentation(text: string): MarkdownPresentation {
  const blocks: MarkdownPresentation = [];
  const lines = text.split('\n');
  let lineIndex = 0;
  while (lineIndex < lines.length) {
    const line = lines[lineIndex];
    const start = recognizeBlock(line);
    if (start?.type === 'pre') {
      const content: string[] = [];
      lineIndex++;
      while (lineIndex < lines.length && !lines[lineIndex].trimStart().startsWith('```')) {
        content.push(lines[lineIndex++]);
      }
      if (lineIndex < lines.length) lineIndex++;
      blocks.push({ type: 'pre', text: content.join('\n') });
      continue;
    }
    if (start?.type === 'heading') {
      blocks.push({ type: 'heading', level: start.level, children: parseInline(start.text) });
      lineIndex++;
      continue;
    }
    if (start?.type === 'list') {
      const prefix = start.ordered ? ORDERED_LIST_RE : UNORDERED_LIST_RE;
      const items: MarkdownInline[][] = [];
      while (lineIndex < lines.length && prefix.test(lines[lineIndex])) {
        items.push(parseInline(lines[lineIndex++].replace(prefix, '')));
      }
      blocks.push({ type: 'list', ordered: start.ordered, items });
      continue;
    }
    if (line.trim() === '') {
      lineIndex++;
      continue;
    }
    const content = [line];
    lineIndex++;
    while (lineIndex < lines.length && lines[lineIndex].trim() !== '' && !recognizeBlock(lines[lineIndex])) {
      content.push(lines[lineIndex++]);
    }
    blocks.push({ type: 'paragraph', children: parseInline(content.join('\n')) });
  }
  return blocks;
}

export function markdownDisplayText(text: string): string {
  const parts: string[] = [];
  let requiredBreaks = 0;
  const append = (content: string, breaks: number) => {
    requiredBreaks = Math.max(requiredBreaks, breaks);
    if (!content) return;
    if (parts.length) parts.push('\n'.repeat(requiredBreaks));
    parts.push(content);
    requiredBreaks = breaks;
  };
  const inlineText = (children: MarkdownInline[]) => children.map(child => child.text).join('');
  const normalText = (children: MarkdownInline[]) => inlineText(children).replace(/[ \t\r\n]+/g, ' ').replace(/^ | $/g, '');
  for (const block of parseMarkdownPresentation(text)) {
    switch (block.type) {
      case 'paragraph':
        append(inlineText(block.children), 2);
        break;
      case 'pre':
        append(block.text, 1);
        break;
      case 'heading':
        append(normalText(block.children), 1);
        break;
      case 'list':
        for (const item of block.items) append(normalText(item), 1);
        break;
    }
  }
  return parts.join('');
}
