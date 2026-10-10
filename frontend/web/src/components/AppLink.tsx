import type { AnchorHTMLAttributes, Ref } from 'react';
import { navigate } from '../route';

export function AppLink({ href, onClick, elementRef, ...props }: AnchorHTMLAttributes<HTMLAnchorElement> & { href: string; elementRef?: Ref<HTMLAnchorElement> }) {
  return <a {...props} ref={elementRef} href={href} onClick={(event) => {
    onClick?.(event);
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey || props.target === '_blank' || props.download) return;
    event.preventDefault();
    navigate(href);
  }} />;
}
