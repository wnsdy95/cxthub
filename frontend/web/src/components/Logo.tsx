// Top brand logo. The app currently uses a single light theme logo.
// (Vite emits hashed separate files for assets over 4KB — no bundling overhead.)
// To introduce a dark theme, swap to cxt-logo-dark-theme.svg using prefers-color-scheme.
import logo from '../assets/cxt-logo-light-theme.svg';

export function Logo({ className }: { className?: string }) {
  return <img src={logo} alt="cxthub" className={className ? `brand-logo ${className}` : 'brand-logo'} />;
}
