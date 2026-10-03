import { createContext, useContext } from 'react';

export interface PaletteControls {
  isOpen: boolean;
  setOpen: (open: boolean) => void;
}

export const PaletteContext = createContext<PaletteControls | null>(null);

export function usePalette(): PaletteControls {
  const v = useContext(PaletteContext);
  if (!v) throw new Error('usePalette must be used inside the app shell');
  return v;
}
