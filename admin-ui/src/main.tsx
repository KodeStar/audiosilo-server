import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from '@/app';
import { createQueryClient } from '@/lib/query-client';
import '@/i18n';
import { createAppRouter } from '@/router';
import './styles/globals.css';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App router={createAppRouter()} queryClient={createQueryClient()} />
  </StrictMode>,
);

// Installable PWA: the root-scoped service worker (/sw.js) the classic console
// registers too. Needs a secure context; failure is harmless.
if ('serviceWorker' in navigator && window.isSecureContext) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js').catch(() => {});
  });
}
