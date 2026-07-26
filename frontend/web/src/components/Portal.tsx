import { type ReactNode } from 'react';
import { createPortal } from 'react-dom';

// Portals the modal/overlay to document.body.
// Ancestors' transform·filter·backdrop-filter (e.g., .landing-header's blur) create a containing block for position:fixed, ensuring the modal is based on the ancestor rather than the viewport (overflowing or clipping).
// Portaling to body avoids this influence, always centering the modal on the screen.
export function Portal({ children }: { children: ReactNode }) {
  return createPortal(children, document.body);
}
