import { Toast } from '@base-ui/react/toast';

/**
 * The app's one toast queue, rendered by <Toaster>. Show one from anywhere
 * (outside React too): `toast.add({ title, description, type })`.
 */
export const toast = Toast.createToastManager();
