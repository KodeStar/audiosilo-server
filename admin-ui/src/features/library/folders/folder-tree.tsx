import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { ChevronRight, Folder, FolderOpen, RotateCw } from 'lucide-react';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';
import { treeKeyAction, type TreeRow } from './folders-model';

// The folder tree: a flat list of treeitems carrying aria-level/setsize/posinset
// (the WAI-ARIA tree pattern), one tab stop that arrow keys move, Right/Left to
// open and close, Enter or Space to select. Hand-built rather than
// @headless-tree because the data is TanStack Query's (the tree and the detail
// panel share the same listings, and one invalidation refreshes both) and the
// tree only needs single selection; the row shaping lives in folders-model.ts.

const ROW_INDENT = 16;

export function FolderTree({
  label,
  rows,
  selected,
  onSelect,
  onToggle,
  onRetry,
}: {
  label: string;
  rows: TreeRow[];
  selected?: string;
  onSelect: (path: string) => void;
  onToggle: (path: string, open: boolean) => void;
  /** Reloads the listing of `parent` after it failed. */
  onRetry: (parent: string) => void;
}) {
  const { t } = useTranslation();
  const items = useRef(new Map<string, HTMLLIElement>());
  const [focused, setFocused] = useState<string>();
  const has = (p?: string) =>
    p !== undefined && rows.some((r) => r.kind === 'folder' && r.entry.path === p);
  const firstFolder = rows.find((r) => r.kind === 'folder');
  // The one tab stop: the row last focused, else the selected one, else the first.
  const tabStop = has(focused)
    ? focused
    : has(selected)
      ? selected
      : firstFolder?.kind === 'folder'
        ? firstFolder.entry.path
        : undefined;

  // Bring the selected folder into view once it shows (a deep link opens its
  // ancestors, which load one level at a time).
  const scrolledTo = useRef<string>(undefined);
  const selectedShown = has(selected);
  useEffect(() => {
    if (!selected || !selectedShown || scrolledTo.current === selected) return;
    scrolledTo.current = selected;
    items.current.get(selected)?.scrollIntoView?.({ block: 'nearest' });
  }, [selected, selectedShown]);

  const onKeyDown = (e: React.KeyboardEvent, path: string) => {
    const action = treeKeyAction(rows, path, e.key);
    if (!action) return;
    e.preventDefault();
    if (action.type === 'focus') {
      setFocused(action.path);
      items.current.get(action.path)?.focus();
    } else if (action.type === 'select') {
      onSelect(action.path);
    } else {
      onToggle(action.path, action.type === 'expand');
    }
  };

  return (
    <ul role="tree" aria-label={label} className="flex flex-col gap-px">
      {rows.map((row) => {
        if (row.kind !== 'folder') {
          return (
            <li
              key={`${row.kind}:${row.parent}`}
              role="none"
              className="flex min-h-8 items-center gap-2 pr-2 text-[12.5px] text-muted-foreground"
              style={{ paddingInlineStart: 8 + (row.level - 1) * ROW_INDENT + 26 }}
            >
              {row.kind === 'loading' ? (
                <span className="skel h-5 w-2/3" role="status" aria-label={t('common.loading')} />
              ) : row.kind === 'error' ? (
                <>
                  <span role="alert">{t('folders.tree.error')}</span>
                  <Button variant="ghost" size="sm" onClick={() => onRetry(row.parent)}>
                    <RotateCw aria-hidden="true" />
                    {t('common.tryAgain')}
                  </Button>
                </>
              ) : (
                <span>{t('folders.tree.more')}</span>
              )}
            </li>
          );
        }
        const { entry, expanded, expandable } = row;
        const isSelected = entry.path === selected;
        const Icon = expanded ? FolderOpen : Folder;
        const badge = entry.override
          ? t(entry.override === 'book' ? 'folders.badge.oneBook' : 'folders.badge.separate')
          : entry.is_book
            ? t('folders.badge.book')
            : undefined;
        return (
          <li
            key={entry.path}
            ref={(el) => {
              if (el) items.current.set(entry.path, el);
              else items.current.delete(entry.path);
            }}
            role="treeitem"
            aria-level={row.level}
            aria-setsize={row.setSize}
            aria-posinset={row.posInSet}
            aria-expanded={expandable ? expanded : undefined}
            aria-selected={isSelected}
            tabIndex={entry.path === tabStop ? 0 : -1}
            onFocus={() => setFocused(entry.path)}
            onKeyDown={(e) => onKeyDown(e, entry.path)}
            onClick={() => {
              onSelect(entry.path);
              if (expandable && !expanded) onToggle(entry.path, true);
            }}
            className={cn(
              'flex min-h-9 cursor-pointer items-center gap-1.5 rounded-sm pr-2 text-[13.5px] transition-colors duration-(--dur-1) select-none hover:bg-muted',
              isSelected &&
                'bg-[color-mix(in_oklab,var(--brand)_9%,transparent)] font-semibold hover:bg-[color-mix(in_oklab,var(--brand)_12%,transparent)]',
            )}
            style={{ paddingInlineStart: 4 + (row.level - 1) * ROW_INDENT }}
          >
            {expandable ? (
              <span
                aria-hidden="true"
                className="grid size-6 shrink-0 place-items-center rounded-sm text-muted-foreground hover:bg-accent hover:text-foreground"
                onClick={(e) => {
                  e.stopPropagation();
                  onToggle(entry.path, !expanded);
                }}
              >
                <ChevronRight
                  className={cn(
                    'size-3.5 transition-transform duration-(--dur-1)',
                    expanded && 'rotate-90',
                  )}
                />
              </span>
            ) : (
              <span className="size-6 shrink-0" aria-hidden="true" />
            )}
            <Icon
              className={cn(
                'size-[15px] shrink-0',
                isSelected ? 'text-brand-ink' : 'text-muted-foreground',
              )}
              aria-hidden="true"
            />
            <span className="min-w-0 flex-1 truncate">{entry.name}</span>
            {badge ? (
              <Badge variant="outline" className="ml-1">
                {badge}
              </Badge>
            ) : null}
          </li>
        );
      })}
    </ul>
  );
}
