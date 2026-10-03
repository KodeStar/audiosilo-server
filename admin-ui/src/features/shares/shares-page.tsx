import { useState } from 'react';
import { useNavigate, useSearch } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import {
  Ellipsis,
  FolderPlus,
  LibraryBig,
  Pencil,
  Plus,
  Share2,
  Trash2,
  UserPlus,
  X,
} from 'lucide-react';
import { api } from '@/api/client';
import { invalidatePeople, useLibraries, useShares, useUsers } from '@/api/hooks';
import type { AdminShare, User } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { EmptyState } from '@/components/empty-state';
import { Monogram } from '@/components/monogram';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { Badge } from '@/components/ui/badge';
import { Button, buttonVariants } from '@/components/ui/button';
import { Card } from '@/components/ui/card';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { toastError } from '@/lib/errors';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import { ruleLabel, shareLabel, wholeLibraryOf } from '@/features/people/people-model';
import { AddFolderDialog } from './add-folder-dialog';
import { ShareNameDialog } from './share-name-dialog';

/**
 * People > Shares: named sets of folders given to people. A share follows its
 * folders, so books added under them appear for everyone with the share.
 * Whole-library grants (made from a person's page) are listed read-only.
 */
export function SharesPage() {
  const { t } = useTranslation();
  const shares = useShares();
  const search = useSearch({ strict: false }) as { share?: number };
  const navigate = useNavigate();
  const [creating, setCreating] = useState(false);

  const all = shares.data ?? [];
  const named = all.filter((s) => wholeLibraryOf(s) === undefined);
  const libraryGrants = all.filter((s) => wholeLibraryOf(s) !== undefined);
  const selected = all.find((s) => s.id === search.share) ?? named[0] ?? libraryGrants[0];
  const select = (id: number) => void navigate({ to: '.', search: { share: id }, replace: true });

  const newButton = (
    <Button onClick={() => setCreating(true)}>
      <Plus aria-hidden="true" />
      {t('shares.new')}
    </Button>
  );

  return (
    <Page>
      <PageHead
        title={t('shares.title')}
        description={t('shares.description')}
        action={newButton}
      />
      {shares.isError ? (
        <QueryError
          title={t('shares.error')}
          error={shares.error}
          onRetry={() => void shares.refetch()}
        />
      ) : !shares.data ? (
        <div className="skel h-[260px] rounded-xl" role="status" aria-label={t('common.loading')} />
      ) : all.length === 0 ? (
        <EmptyState
          icon={Share2}
          title={t('shares.empty.title')}
          body={t('shares.empty.body')}
          action={newButton}
        />
      ) : (
        <div className="grid items-start gap-5 lg:grid-cols-[minmax(0,360px)_minmax(0,1fr)]">
          <nav aria-label={t('shares.listLabel')} className="flex flex-col gap-2.5">
            {named.map((s) => (
              <ShareTile key={s.id} share={s} selected={s.id === selected?.id} onSelect={select} />
            ))}
            {libraryGrants.length ? (
              <>
                <h2 className="eyebrow mt-3 px-1">{t('shares.libraryGrants')}</h2>
                {libraryGrants.map((s) => (
                  <ShareTile
                    key={s.id}
                    share={s}
                    selected={s.id === selected?.id}
                    onSelect={select}
                  />
                ))}
              </>
            ) : null}
          </nav>
          {selected ? <ShareDetail key={selected.id} share={selected} /> : null}
        </div>
      )}
      <ShareNameDialog open={creating} onOpenChange={setCreating} onCreated={select} />
    </Page>
  );
}

function ShareTile({
  share: s,
  selected,
  onSelect,
}: {
  share: AdminShare;
  selected: boolean;
  onSelect: (id: number) => void;
}) {
  const { t } = useTranslation();
  const users = useUsers();
  const libraries = useLibraries();
  const members = (users.data ?? []).filter((u) => s.member_ids.includes(u.id));
  const isLibrary = wholeLibraryOf(s) !== undefined;
  const name = shareLabel(s, libraries.data ?? []);
  return (
    <button
      type="button"
      onClick={() => onSelect(s.id)}
      aria-current={selected ? 'true' : undefined}
      className={cn(
        'flex items-center gap-3.5 rounded-xl border bg-card p-4 text-left transition-colors duration-(--dur-1) hover:border-border-strong',
        selected && 'border-foreground shadow-[0_0_0_1px_var(--foreground)]',
      )}
    >
      <span
        className="grid size-10 shrink-0 place-items-center rounded-[12px] bg-muted text-muted-foreground"
        aria-hidden="true"
      >
        {isLibrary ? <LibraryBig className="size-5" /> : <Share2 className="size-5" />}
      </span>
      <span className="flex min-w-0 flex-1 flex-col">
        <b className="truncate text-[15px]">{name}</b>
        <span className="text-[12.5px] text-muted-foreground">
          {isLibrary
            ? t('shares.wholeLibrary')
            : t('shares.folderCount', { count: s.paths?.length ?? 0 })}
          {' · '}
          {t('shares.peopleCount', { count: s.member_ids.length })}
        </span>
      </span>
      {members.length ? (
        <span className="flex shrink-0" aria-hidden="true">
          {members.slice(0, 4).map((m, i) => (
            <span key={m.id} className={cn('rounded-full ring-2 ring-card', i > 0 && '-ml-2')}>
              <Monogram name={m.username} size={26} />
            </span>
          ))}
        </span>
      ) : null}
    </button>
  );
}

function ShareDetail({ share: s }: { share: AdminShare }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const libraries = useLibraries();
  const users = useUsers();
  const navigate = useNavigate();
  const [dialog, setDialog] = useState<'rename' | 'delete' | 'folder' | null>(null);
  const isLibrary = wholeLibraryOf(s) !== undefined;
  const libs = libraries.data ?? [];
  const people = users.data ?? [];
  const members = people.filter((u) => s.member_ids.includes(u.id));
  const others = people.filter((u) => !s.member_ids.includes(u.id) && u.role !== 'admin');
  const name = shareLabel(s, libs);

  const run = async (work: () => Promise<void>, done: string) => {
    try {
      await work();
      invalidatePeople(qc);
      toast.add({ title: done, type: 'success' });
    } catch (err) {
      toastError(t('shares.toast.failed'), err);
    }
  };

  return (
    <Card aria-labelledby="share-title">
      <div className="flex flex-wrap items-start justify-between gap-3 border-b px-5 py-4">
        <div className="flex min-w-0 flex-col gap-1">
          <div className="flex flex-wrap items-center gap-2">
            <h2 id="share-title" className="h2 [overflow-wrap:anywhere]">
              {name}
            </h2>
            {isLibrary ? <Badge variant="outline">{t('shares.wholeLibrary')}</Badge> : null}
          </div>
          {isLibrary ? (
            <span className="text-[12.5px] text-muted-foreground">
              {t('shares.libraryGrantHint')}
            </span>
          ) : null}
        </div>
        {!isLibrary ? (
          <DropdownMenu>
            <DropdownMenuTrigger
              className={buttonVariants({ variant: 'ghost', size: 'icon-sm' })}
              aria-label={t('shares.actions', { name: s.name })}
            >
              <Ellipsis className="size-4" aria-hidden="true" />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem onClick={() => setDialog('rename')}>
                <Pencil aria-hidden="true" />
                {t('shares.rename')}
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem variant="destructive" onClick={() => setDialog('delete')}>
                <Trash2 aria-hidden="true" />
                {t('shares.delete')}
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        ) : null}
      </div>

      <section aria-labelledby="share-folders" className="flex flex-col gap-2.5 px-5 py-4">
        <h3 id="share-folders" className="text-[13px] font-semibold">
          {t('shares.folders')}
        </h3>
        {(s.paths ?? []).length === 0 ? (
          <p className="text-[13px] text-muted-foreground">{t('shares.noFolders')}</p>
        ) : (
          <ul className="flex flex-col gap-2">
            {(s.paths ?? []).map((r) => {
              const label = ruleLabel(r, libs);
              return (
                <li key={`${r.library_id}:${r.path}`} className="flex items-center gap-2.5">
                  <span className="flex min-w-0 flex-1 items-center gap-2 rounded-md border bg-muted/50 px-3 py-2 font-mono text-[12.5px]">
                    <span className="min-w-0 truncate">{label}</span>
                    {!r.path ? (
                      <span className="shrink-0 font-sans text-muted-foreground">
                        {t('shares.wholeLibraryRule')}
                      </span>
                    ) : null}
                  </span>
                  {!isLibrary ? (
                    <Button
                      variant="ghost"
                      size="icon-sm"
                      aria-label={t('shares.removeFolder', { what: label })}
                      onClick={() =>
                        void run(
                          () => api.removeSharePath(s.id, r),
                          t('shares.toast.folderRemoved', { what: label }),
                        )
                      }
                    >
                      <X className="size-4" aria-hidden="true" />
                    </Button>
                  ) : null}
                </li>
              );
            })}
          </ul>
        )}
        {!isLibrary ? (
          <div>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setDialog('folder')}
              disabled={!libs.length}
            >
              <FolderPlus aria-hidden="true" />
              {t('shares.addFolder')}
            </Button>
          </div>
        ) : null}
      </section>

      <section aria-labelledby="share-people" className="flex flex-col gap-2.5 border-t px-5 py-4">
        <h3 id="share-people" className="text-[13px] font-semibold">
          {t('shares.people')}
        </h3>
        {members.length === 0 ? (
          <p className="text-[13px] text-muted-foreground">{t('shares.noPeople')}</p>
        ) : null}
        <ul className="flex flex-wrap gap-2">
          {members.map((m) => (
            <li
              key={m.id}
              className="inline-flex h-9 items-center gap-2 rounded-full border bg-card pr-1 pl-1"
            >
              <Monogram name={m.username} size={28} />
              <span className="text-[13px] font-[550]">{m.username}</span>
              {/* A whole-library grant is changed from each person's page. */}
              {!isLibrary ? (
                <Button
                  variant="ghost"
                  size="icon-sm"
                  className="size-7 rounded-full"
                  aria-label={t('shares.removePerson', { name: m.username })}
                  onClick={() =>
                    void run(
                      () => api.revokeShare(m.id, s.id),
                      t('shares.toast.personRemoved', { name: m.username, share: name }),
                    )
                  }
                >
                  <X className="size-3.5" aria-hidden="true" />
                </Button>
              ) : (
                <span className="w-1.5" />
              )}
            </li>
          ))}
          {others.length && !isLibrary ? (
            <li>
              <AddPersonMenu
                people={others}
                onPick={(u) =>
                  void run(
                    () => api.grantShare(u.id, s.id),
                    t('shares.toast.personAdded', { name: u.username, share: name }),
                  )
                }
              />
            </li>
          ) : null}
        </ul>
      </section>

      <ShareNameDialog
        share={s}
        open={dialog === 'rename'}
        onOpenChange={(o) => setDialog(o ? 'rename' : null)}
      />
      <AddFolderDialog
        share={s}
        open={dialog === 'folder'}
        onOpenChange={(o) => setDialog(o ? 'folder' : null)}
      />
      <ConfirmDialog
        open={dialog === 'delete'}
        onOpenChange={(o) => setDialog(o ? 'delete' : null)}
        icon={Trash2}
        title={t('shares.deleteTitle', { name: s.name })}
        description={t('shares.deleteBody', { count: s.member_ids.length })}
        confirmLabel={t('shares.delete')}
        onConfirm={async () => {
          await api.deleteShare(s.id);
          invalidatePeople(qc);
          toast.add({ title: t('shares.toast.deleted', { name: s.name }), type: 'success' });
          void navigate({ to: '.', search: {}, replace: true });
        }}
      />
    </Card>
  );
}

function AddPersonMenu({ people, onPick }: { people: User[]; onPick: (u: User) => void }) {
  const { t } = useTranslation();
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        className={cn(buttonVariants({ variant: 'outline', size: 'sm' }), 'h-9 rounded-full')}
      >
        <UserPlus aria-hidden="true" />
        {t('shares.addPerson')}
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start">
        {people.map((u) => (
          <DropdownMenuItem key={u.id} onClick={() => onPick(u)}>
            <Monogram name={u.username} size={22} />
            {u.username}
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
