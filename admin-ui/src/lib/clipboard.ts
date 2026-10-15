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
  try {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.className = 'fixed -left-[9999px] opacity-0';
    document.body.append(ta);
    ta.select();
    const ok = document.execCommand('copy');
    ta.remove();
    return ok;
  } catch {
    return false;
  }
}

/**
 * Copies text and says how it went in a toast: "<what> copied", or, when the
 * browser refused, a warning that shows the text to copy by hand.
 */
export async function copyWithToast(text: string, what: string): Promise<void> {
  const t = i18n.t;
  if (await copyText(text)) toast.add({ title: t('common.copied', { what }), type: 'success' });
  else toast.add({ title: t('common.copyFailed'), description: text, type: 'warning' });
}
