import { api } from '@/api/client';
import type { AccessChoice } from './people-model';

/** Gives a person a whole library or a share (POST /admin/library-access or /share-access). */
export function grantAccess(userId: number, choice: AccessChoice) {
  return choice.kind === 'library'
    ? api.grantLibrary(userId, choice.id)
    : api.grantShare(userId, choice.id);
}
