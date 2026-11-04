import React from 'react';
import { parseMarkdownPresentation, type MarkdownBlock, type MarkdownInline } from '../markdownPresentation';
import { safeHttpUrl } from '../urls';

function renderInline(children: MarkdownInline[]): React.ReactNode[] {
  return children.map((child, index) => {
    switch (child.type) {
      case 'text':
        return child.text;
      case 'strong':
        return <strong key={index}>{child.text}</strong>;
      case 'code':
        return <code key={index}>{child.text}</code>;
      case 'link': {
        const href = safeHttpUrl(child.href);
        return href ? (
          <a key={index} href={href} target="_blank" rel="noopener noreferrer">{child.text}</a>
        ) : child.text;
      }
    }
  });
}

function renderBlock(block: MarkdownBlock, index: number): React.ReactNode {
  switch (block.type) {
    case 'paragraph':
      return <p key={index}>{renderInline(block.children)}</p>;
    case 'pre':
      return <pre key={index}>{block.text}</pre>;
    case 'heading': {
      const Tag = (['h4', 'h5', 'h6'] as const)[block.level - 1];
      return <Tag key={index}>{renderInline(block.children)}</Tag>;
    }
    case 'list': {
      const Tag = block.ordered ? 'ol' : 'ul';
      return <Tag key={index}>{block.items.map((item, itemIndex) => <li key={itemIndex}>{renderInline(item)}</li>)}</Tag>;
    }
  }
}

export function Markdown({ text }: { text: string }) {
  return <div className="md">{parseMarkdownPresentation(text).map(renderBlock)}</div>;
}
