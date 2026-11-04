import { createContext, useContext } from 'react';
import type { QueryKey } from '@tanstack/react-query';

export const DocumentReadScope = createContext<QueryKey | null>(null);

export function useDocumentReadScope() {
  return useContext(DocumentReadScope);
}
