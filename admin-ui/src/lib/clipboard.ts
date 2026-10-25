import i18n from 'i18next';
import { toast } from './toast';

/**
 * Copies text, returning whether it worked. The async Clipboard API needs a
 * secure context; on plain HTTP (a LAN address) fall back to a hidden textarea.
 */
export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    // fall through to the legacy path
  }
  const ta = document.createElement('textarea');
  // The textarea takes the focus to be selected: give it back after (to the
  // Copy button, for a keyboard user).
  const focused = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  // Placed next to that element, not at the end of <body>: inside an open
  // dialog or menu, focus leaving the popup would trip its focus handling (a
  // non-modal menu closes on focus-out and then skips returning focus).
  const host = focused && focused !== document.body ? focused.parentElement : null;
  try {
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    // top too: inside a transformed popup "fixed" is relative to the popup, and
    // an unset top would leave it at the end of the host (a scroll target).
    ta.style.top = '0';
    ta.style.left = '-9999px';
    ta.style.opacity = '0';
    (host ?? document.body).append(ta);
    ta.focus({ preventScroll: true });
    ta.select();
    // iOS Safari ignores select() on a readonly textarea.
    ta.setSelectionRange(0, text.length);
    return document.execCommand('copy');
  } catch {
    return false;
  } finally {
    // Always taken out again, also when execCommand throws.
    ta.remove();
    focused?.focus({ preventScroll: true });
  }
}

/**
 * Copies text and says how it went in a toast: "<what> copied", or, when the
 * browser refused, a warning that shows the text to copy by hand.
 */
export async function copyWithToast(text: string, what: string): Promise<void> {
  const t = i18n.t;
  if (await copyText(text)) toast.add({ title: t('common.copied', { what }), type: 'success' });
  // No timeout: it stays until closed, so there is time to select the text.
  else toast.add({ title: t('common.copyFailed'), description: text, type: 'warning', timeout: 0 });
}
