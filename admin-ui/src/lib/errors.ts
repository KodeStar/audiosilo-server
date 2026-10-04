import i18n, { type TFunction } from 'i18next';
import { ApiError } from '@/api/client';
import { toast } from './toast';

// Failures a person can fix carry a machine-readable code from the server
// (respond.go); those get localized copy that says how. Anything else is shown
// as the server's message, which is still specific (invalid_override among
// them: the server's sentence names the field and the rule, and the book page
// maps it onto the field itself).
const BY_CODE: Record<string, string> = {
  username_taken: 'errors.usernameTaken',
  name_taken: 'errors.nameTaken',
  last_admin: 'errors.lastAdmin',
  admin_needs_password: 'errors.adminNeedsPassword',
  password_too_short: 'errors.passwordTooShort',
  cannot_delete_self: 'errors.deleteSelf',
  folder_unreadable: 'errors.folderUnreadable',
  // The console caps bulk edits and share additions at the server's limits, so
  // the one too_large a person can hit is a cover upload.
  too_large: 'book.cover.tooLarge',
  unsupported_image: 'book.cover.unsupported',
  metadata_off: 'book.match.off',
  book_not_found: 'errors.bookNotFound',
  no_access: 'errors.noAccess',
  current_device: 'errors.currentDevice',
};

/** A failure as one sentence for a toast or a field. */
export function errorMessage(err: unknown, t: TFunction = i18n.t): string {
  if (err instanceof ApiError) {
    const key = err.code ? BY_CODE[err.code] : undefined;
    if (key) return t(key);
    if (err.status === 429) return t('auth.rateLimited');
    return err.message;
  }
  return t('auth.unreachable');
}

/** Toasts a failed action: what didn't happen, and why. */
export function toastError(title: string, err: unknown) {
  toast.add({ title, description: errorMessage(err), type: 'error' });
}

/**
 * A form field's message: schema messages are i18n keys (translated here), and
 * server errors set on a field are already sentences.
 */
export function fieldMessage(message: string | undefined, t: TFunction): string | undefined {
  if (!message) return undefined;
  return i18n.exists(message) ? t(message) : message;
}
