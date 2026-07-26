// State — Only holds the client UI state.
//
// Authentication state is no longer stored in JS (localStorage/memory token).
// Session tokens exist only as HttpOnly cookies (JS inaccessible), and "logged in" status is determined by the success of the React Query `me` query (cookie as the single source of truth). → The store only holds the screen selection state.
import { create } from 'zustand';

interface UiState {
  selectedWorkspaceId: string | null;
  selectWorkspace: (id: string | null) => void;
}

export const useUiStore = create<UiState>((set) => ({
  selectedWorkspaceId: null,
  selectWorkspace: (id) => set({ selectedWorkspaceId: id }),
}));
