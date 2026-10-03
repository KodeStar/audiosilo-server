import { useEffect, useMemo, useState } from 'react';
import { Outlet } from '@tanstack/react-router';
import { CommandPalette } from './command-palette';
import { PaletteContext, type PaletteControls } from './palette-context';
import { TabBar } from './tab-bar';
import { TopBar } from './top-bar';

function isEditable(el: Element | null): boolean {
  if (!el) return false;
  if (el instanceof HTMLElement && el.isContentEditable) return true;
  return /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName);
}

/** The signed-in console: top bar, the routed page, mobile tab bar, ⌘K palette. */
export function AppShell() {
  const [isOpen, setOpen] = useState(false);
  const palette = useMemo<PaletteControls>(
    () => ({ isOpen, setOpen, open: () => setOpen(true) }),
    [isOpen],
  );

  // ⌘K / Ctrl+K toggles the palette anywhere; "/" opens it when not typing.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        setOpen((o) => !o);
      } else if (
        e.key === '/' &&
        !e.metaKey &&
        !e.ctrlKey &&
        !e.altKey &&
        !isEditable(document.activeElement)
      ) {
        e.preventDefault();
        setOpen(true);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  return (
    <PaletteContext.Provider value={palette}>
      <TopBar />
      <main id="main">
        <Outlet />
      </main>
      <TabBar />
      <CommandPalette />
    </PaletteContext.Provider>
  );
}
