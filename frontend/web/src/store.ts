// State — Only holds the client UI state.
//
// Authentication state is no longer stored in JS (localStorage/memory token).
// Session tokens exist only as HttpOnly cookies (JS inaccessible), and "logged in" status is determined by the success of the React Query `me` query (cookie as the single source of truth). → The store only holds the screen selection state.
import { create } from 'zustand';

interface UiState {
  selectedRepositoryId: string | null;
  selectRepository: (id: string | null) => void;
}

export const useUiStore = create<UiState>((set) => ({
  selectedRepositoryId: null,
  selectRepository: (id) => set({ selectedRepositoryId: id }),
}));
