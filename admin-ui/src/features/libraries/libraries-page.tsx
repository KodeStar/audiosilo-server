import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import {
  DndContext,
  KeyboardSensor,
  PointerSensor,
  closestCenter,
  useSensor,
  useSensors,
  type Announcements,
  type DragEndEvent,
} from '@dnd-kit/core';
import {
  SortableContext,
  arrayMove,
  sortableKeyboardCoordinates,
  verticalListSortingStrategy,
} from '@dnd-kit/sortable';
import { LibraryBig, Plus } from 'lucide-react';
import { api } from '@/api/client';
import { keys, useLibraries, useScanFinished } from '@/api/hooks';
import type { AdminLibrary } from '@/api/types';
import { EmptyState } from '@/components/empty-state';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { Button } from '@/components/ui/button';
import { toastError } from '@/lib/errors';
import { useSearchDialog } from '@/lib/use-search-dialog';
import { toast } from '@/lib/toast';
import { LibraryCard } from './library-card';
import { LibraryFormDialog } from './library-form-dialog';

/**
 * Library > Libraries: the folders AudioSilo reads, in the order players list
 * them (which also decides which copy of a duplicated book wins). Add, edit,
 * reorder, rescan, export, correct folder detection, delete.
 */
export function LibrariesPage() {
  const { t } = useTranslation();
  const libraries = useLibraries();
  const [addOpen, setAddOpen] = useSearchDialog('add');
  useScanFinished((l) =>
    l.scan.unavailable
      ? toast.add({
          title: t('libraries.toast.scanStopped', { name: l.name }),
          description: t('libraries.toast.scanStoppedBody'),
          type: 'warning',
        })
      : toast.add({ title: t('libraries.toast.scanFinished', { name: l.name }), type: 'success' }),
  );

  const addButton = (
    <Button onClick={() => setAddOpen(true)}>
      <Plus aria-hidden="true" />
      {t('libraries.add')}
    </Button>
  );

  return (
    <Page>
      <PageHead
        title={t('libraries.title')}
        description={t('libraries.description')}
        action={addButton}
      />
      {libraries.isError ? (
        <QueryError
          title={t('libraries.error')}
          error={libraries.error}
          onRetry={() => void libraries.refetch()}
        />
      ) : !libraries.data ? (
        <div className="flex flex-col gap-3.5" role="status" aria-label={t('common.loading')}>
          {[0, 1].map((i) => (
            <div key={i} className="skel h-[118px] rounded-xl" />
          ))}
        </div>
      ) : libraries.data.length === 0 ? (
        <EmptyState
          icon={LibraryBig}
          title={t('libraries.empty.title')}
          body={t('libraries.empty.body')}
          action={addButton}
        />
      ) : (
        <SortableLibraries libraries={libraries.data} />
      )}
      <LibraryFormDialog open={addOpen} onOpenChange={setAddOpen} />
    </Page>
  );
}

function SortableLibraries({ libraries }: { libraries: AdminLibrary[] }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 4 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  );
  const nameOf = (id: string | number) => libraries.find((l) => l.id === Number(id))?.name ?? '';
  const positionOf = (id: string | number) => libraries.findIndex((l) => l.id === Number(id)) + 1;

  const move = (from: number, to: number) => {
    if (to < 0 || to >= libraries.length || from === to) return;
    const next = arrayMove(libraries, from, to);
    // Show the new order at once; the server's answer (or a refetch) settles it.
    qc.setQueryData(keys.libraries, next);
    api.reorderLibraries(next.map((l) => l.id)).then(
      // The answer is the whole list, freshly probed: no refetch needed.
      (res) => qc.setQueryData(keys.libraries, res.libraries),
      (err: unknown) => {
        void qc.invalidateQueries({ queryKey: keys.libraries });
        toastError(t('libraries.toast.reorderFailed'), err);
      },
    );
  };

  const onDragEnd = ({ active, over }: DragEndEvent) => {
    if (!over || active.id === over.id) return;
    move(positionOf(active.id) - 1, positionOf(over.id) - 1);
  };

  const announcements: Announcements = {
    onDragStart: ({ active }) => t('libraries.dnd.start', { name: nameOf(active.id) }),
    onDragOver: ({ active, over }) =>
      over
        ? t('libraries.dnd.over', { name: nameOf(active.id), position: positionOf(over.id) })
        : undefined,
    onDragEnd: ({ active, over }) =>
      over
        ? t('libraries.dnd.end', { name: nameOf(active.id), position: positionOf(over.id) })
        : undefined,
    onDragCancel: ({ active }) => t('libraries.dnd.cancel', { name: nameOf(active.id) }),
  };

  return (
    <>
      <DndContext
        sensors={sensors}
        collisionDetection={closestCenter}
        onDragEnd={onDragEnd}
        accessibility={{
          announcements,
          screenReaderInstructions: { draggable: t('libraries.dnd.instructions') },
        }}
      >
        <SortableContext items={libraries.map((l) => l.id)} strategy={verticalListSortingStrategy}>
          <ol className="flex flex-col gap-3.5" aria-label={t('libraries.listLabel')}>
            {libraries.map((l, i) => (
              <LibraryCard
                key={l.id}
                library={l}
                onMoveUp={i > 0 ? () => move(i, i - 1) : undefined}
                onMoveDown={i < libraries.length - 1 ? () => move(i, i + 1) : undefined}
              />
            ))}
          </ol>
        </SortableContext>
      </DndContext>
      {libraries.length > 1 ? (
        <p className="mt-4 text-[12.5px] text-muted-foreground">{t('libraries.orderHint')}</p>
      ) : null}
    </>
  );
}
