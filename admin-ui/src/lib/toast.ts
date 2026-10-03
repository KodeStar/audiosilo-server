import { Toast } from '@base-ui/react/toast';

/** The app's one toast queue (rendered by <Toaster>). Usable outside React too. */
export const toastManager = Toast.createToastManager();

/** Shows a toast: `toast.add({ title, description, type })`. */
export const toast = toastManager;
