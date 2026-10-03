import { useState } from 'react';
import { useNavigate, useSearch } from '@tanstack/react-router';

/**
 * A dialog that a URL flag can open (`?add=1`, `?invite=1`: the first-run call
 * to action and the palette's actions). Closing it drops the flag, so Back and
 * a reload don't reopen it.
 */
export function useSearchDialog(flag: 'add' | 'invite'): [boolean, (open: boolean) => void] {
  const search = useSearch({ strict: false }) as Partial<Record<typeof flag, true>>;
  const navigate = useNavigate();
  const [local, setLocal] = useState(false);
  const setOpen = (open: boolean) => {
    setLocal(open);
    if (!open && search[flag]) void navigate({ to: '.', search: {}, replace: true });
  };
  return [local || search[flag] === true, setOpen];
}
