import { createContext, useContext, useEffect } from 'react';

export const DraftsContext = createContext<((id: string, dirty: boolean) => void) | undefined>(
  undefined,
);

export function useSettingsDraft(id: string, dirty: boolean) {
  const register = useContext(DraftsContext);
  useEffect(() => {
    register?.(id, dirty);
    return () => register?.(id, false);
  }, [register, id, dirty]);
}
