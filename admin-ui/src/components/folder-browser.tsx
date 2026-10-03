import { useTranslation } from 'react-i18next';
import { BookOpen, ChevronRight, CornerLeftUp, Folder, Headphones } from 'lucide-react';
import type { Crumb } from '@/lib/paths';
import { cn } from '@/lib/utils';

// One folder at a time with a breadcrumb: the shape all three pickers share (a
// share's folders, folder-detection overrides, a new library's root). Callers
// supply the rows and what each row offers; this owns the layout and keyboard.

export interface FolderRow {
  path: string;
  name: string;
  kind: 'dir' | 'book' | 'audio';
  /** A muted second line (an author, "Detected as one book"). */
  detail?: React.ReactNode;
  /** Opens the folder when clicked (dirs only, and only where browsing deeper means something). */
  openable?: boolean;
}

const ICONS = { dir: Folder, book: BookOpen, audio: Headphones } as const;

export function FolderBrowser({
  crumbs,
  onNavigate,
  rows,
  action,
  loading,
  error,
  emptyText,
  current,
  label,
}: {
  crumbs: Crumb[];
  onNavigate: (path: string) => void;
  rows: FolderRow[];
  /** What a row offers on the right (a button, a select). */
  action?: (row: FolderRow) => React.ReactNode;
  loading?: boolean;
  error?: string;
  emptyText: string;
  /** Pinned first row for the folder being shown ("Share this folder"). */
  current?: React.ReactNode;
  /** Accessible name of the list. */
  label: string;
}) {
  const { t } = useTranslation();
  const parent = crumbs.length > 1 ? crumbs[crumbs.length - 2] : undefined;
  return (
    <div className="flex min-w-0 flex-col overflow-hidden rounded-[12px] border">
      <nav
        aria-label={t('picker.breadcrumb')}
        className="hscroll flex items-center gap-1 border-b bg-muted/60 px-2.5 py-2"
      >
        {parent ? (
          <button
            type="button"
            onClick={() => onNavigate(parent.path)}
            className="mr-1 grid size-7 shrink-0 place-items-center rounded-sm text-muted-foreground hover:bg-accent hover:text-foreground"
            aria-label={t('picker.up')}
          >
            <CornerLeftUp className="size-4" aria-hidden="true" />
          </button>
        ) : null}
        <ol className="flex items-center gap-0.5 font-mono text-[12.5px] whitespace-nowrap">
          {crumbs.map((c, i) => {
            const last = i === crumbs.length - 1;
            return (
              <li key={c.path || '/'} className="flex items-center gap-0.5">
                {i > 0 ? (
                  <ChevronRight className="size-3.5 text-subtle-foreground" aria-hidden="true" />
                ) : null}
                {last ? (
                  <span aria-current="location" className="px-1 font-semibold">
                    {c.label}
                  </span>
                ) : (
                  <button
                    type="button"
                    onClick={() => onNavigate(c.path)}
                    className="rounded-sm px-1 text-muted-foreground hover:bg-accent hover:text-foreground"
                  >
                    {c.label}
                  </button>
                )}
              </li>
            );
          })}
        </ol>
      </nav>
      {current ? <div className="border-b px-3 py-2.5">{current}</div> : null}
      <div className="max-h-[340px] min-h-[120px] overflow-y-auto">
        {loading ? (
          <div className="flex flex-col gap-2 p-3" role="status" aria-label={t('common.loading')}>
            {[0, 1, 2, 3].map((i) => (
              <div key={i} className="skel h-9" />
            ))}
          </div>
        ) : error ? (
          <p role="alert" className="px-4 py-6 text-center text-destructive">
            {error}
          </p>
        ) : rows.length === 0 ? (
          <p className="px-4 py-6 text-center text-muted-foreground">{emptyText}</p>
        ) : (
          <ul aria-label={label} className="divide-y">
            {rows.map((row) => {
              const Icon = ICONS[row.kind];
              const name = (
                <>
                  <Icon
                    className={cn(
                      'size-4 shrink-0',
                      row.kind === 'book' ? 'text-brand-ink' : 'text-muted-foreground',
                    )}
                    aria-hidden="true"
                  />
                  <span className="flex min-w-0 flex-col">
                    <span className="truncate font-[550]">{row.name}</span>
                    {row.detail ? (
                      <span className="truncate text-[12px] text-muted-foreground">
                        {row.detail}
                      </span>
                    ) : null}
                  </span>
                </>
              );
              return (
                <li key={row.path} className="flex min-h-11 items-center gap-2 pr-2.5">
                  {row.openable ? (
                    <button
                      type="button"
                      onClick={() => onNavigate(row.path)}
                      className="flex min-h-11 min-w-0 flex-1 items-center gap-2.5 py-1.5 pl-3 text-left hover:bg-accent/60"
                    >
                      {name}
                    </button>
                  ) : (
                    <span className="flex min-h-11 min-w-0 flex-1 items-center gap-2.5 py-1.5 pl-3">
                      {name}
                    </span>
                  )}
                  {action ? <span className="shrink-0">{action(row)}</span> : null}
                </li>
              );
            })}
          </ul>
        )}
      </div>
    </div>
  );
}
