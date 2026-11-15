import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from '@/app';
import { createQueryClient } from '@/lib/query-client';
import { i18nReady } from '@/i18n';
import { takeOIDCReturn } from '@/lib/oidc-return';
import { createAppRouter } from '@/router';
import './styles/globals.css';

// A single sign-on's return (/admin/#oidc=done): read and out of the address
// bar before the router reads the location, so no navigation writes it back.
const oidcDone = takeOIDCReturn();

// Render once the console's language is loaded (a separate chunk unless English).
void i18nReady.then(() =>
  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      <App router={createAppRouter()} queryClient={createQueryClient()} oidcDone={oidcDone} />
    </StrictMode>,
  ),
);

// Installable PWA: the root-scoped service worker (/sw.js) the server ships.
// Needs a secure context; failure is harmless.
if ('serviceWorker' in navigator && window.isSecureContext) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js').catch(() => {});
  });
}
