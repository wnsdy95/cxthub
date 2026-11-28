import { createContext, useContext, useLayoutEffect, useRef, useState, type ComponentProps, type Dispatch, type ReactNode, type SetStateAction } from 'react';

const Presentation = createContext<Map<string, unknown> | null>(null);
const EventIdentity = createContext('');

export function DocumentPresentation({ children }: { children: ReactNode }) {
  const values = useRef(new Map<string, unknown>());
  return <Presentation.Provider value={import.meta.env.VITE_DOCUMENT_RENDER_WINDOW === '1' ? values.current : null}>{children}</Presentation.Provider>;
}

export function DocumentEventIdentity({ index, children }: { index: number; children: ReactNode }) {
  return <EventIdentity.Provider value={`event:${index}:`}>{children}</EventIdentity.Provider>;
}

export function useDocumentPresentation<Value>(name: string, initial: Value): [Value, Dispatch<SetStateAction<Value>>] {
  const values = useContext(Presentation);
  const key = useContext(EventIdentity) + name;
  const [value, setValue] = useState<Value>(() => values?.has(key) ? values.get(key) as Value : initial);
  const current = useRef(value);
  return [value, update => {
    const next = typeof update === 'function' ? (update as (previous: Value) => Value)(current.current) : update;
    current.current = next;
    values?.set(key, next);
    setValue(next);
  }];
}

export function DocumentDisclosure({ name, onToggle, ...props }: ComponentProps<'details'> & { name: string }) {
  const [open, setOpen] = useDocumentDisclosureState(name);
  return <details {...props} open={open} onToggle={event => {
    setOpen(event.currentTarget.open);
    onToggle?.(event);
  }} />;
}

export function useDocumentDisclosureState(name: string): [boolean, Dispatch<SetStateAction<boolean>>] {
  return useDocumentPresentation(name, false);
}

export function DocumentScrollText({ name, ...props }: ComponentProps<'pre'> & { name: string }) {
  const values = useContext(Presentation);
  const key = useContext(EventIdentity) + name;
  const container = useRef<HTMLPreElement>(null);
  useLayoutEffect(() => {
    const node = container.current;
    if (!node) return;
    node.scrollTop = values?.get(key) as number ?? 0;
    const remember = () => { values?.set(key, node.scrollTop); };
    node.addEventListener('scroll', remember, { passive: true });
    return () => node.removeEventListener('scroll', remember);
  }, [key, values]);
  return <pre {...props} ref={container} />;
}
