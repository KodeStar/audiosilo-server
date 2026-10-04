import { useEffect, useState } from 'react';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { z } from '@/lib/zod';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Database, FolderSearch } from 'lucide-react';
import { ApiError, api } from '@/api/client';
import { invalidateLibraries, noteScanStarted, useDirs, useLibraries } from '@/api/hooks';
import type { AdminLibrary, Job, Library } from '@/api/types';
import { FolderBrowser } from '@/components/folder-browser';
import { Button } from '@/components/ui/button';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { Field } from '@/components/ui/field';
import { describedBy } from '@/lib/a11y';
import { Input } from '@/components/ui/input';
import { NativeSelect } from '@/components/ui/native-select';
import { Textarea } from '@/components/ui/textarea';
import { errorMessage, fieldMessage } from '@/lib/errors';
import { absoluteBaseName, absoluteCrumbs, isAbsolutePath } from '@/lib/paths';
import { toast } from '@/lib/toast';
import {
  SCHEDULE_CHOICES,
  joinSchedule,
  patternsToText,
  scheduleLabel,
  splitSchedule,
  textToPatterns,
  type ScheduleChoice,
} from './scan-settings';

// Messages are i18n keys, translated where they're shown.
const schema = z.object({
  name: z.string().trim().min(1, 'libraries.form.nameRequired'),
  // Not `root`: react-hook-form reserves errors.root for form-level errors.
  folder: z.string().trim().min(1, 'libraries.form.folderRequired'),
  schedule: z.enum(SCHEDULE_CHOICES as [ScheduleChoice, ...ScheduleChoice[]]),
  time: z.string(),
  ignore: z.string(),
});
type Values = z.infer<typeof schema>;

/**
 * Add a library, or (with `library`) rename it or point it at another folder.
 * The folder can be typed or picked from the server's own folders.
 */
export function LibraryFormDialog({
  library,
  open,
  onOpenChange,
}: {
  library?: AdminLibrary;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        size="lg"
        icon={Database}
        title={
          library
            ? t('libraries.form.editTitle', { name: library.name })
            : t('libraries.form.addTitle')
        }
        description={
          library ? t('libraries.form.editDescription') : t('libraries.form.addDescription')
        }
      >
        {open ? <LibraryForm library={library} onDone={() => onOpenChange(false)} /> : null}
      </DialogContent>
    </Dialog>
  );
}

function LibraryForm({ library, onDone }: { library?: AdminLibrary; onDone: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [browsing, setBrowsing] = useState(false);
  const current = splitSchedule(library?.scan_schedule ?? '');
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: {
      name: library?.name ?? '',
      folder: library?.root ?? '',
      schedule: current.choice,
      time: current.time,
      ignore: patternsToText(library?.ignore_patterns ?? []),
    },
  });
  const { errors, isSubmitting } = form.formState;

  const onSubmit = form.handleSubmit(async (v) => {
    const patterns = textToPatterns(v.ignore);
    const lib = {
      name: v.name,
      root: v.folder,
      scan_schedule: joinSchedule(v.schedule, v.time),
      ignore_patterns: patterns,
    };
    try {
      // A new library is always scanned; an edit only when the server queued a
      // scan for it (a new folder or new ignore rules).
      const saved: Library & { job?: Job } = library
        ? await api.updateLibrary(library.id, lib)
        : await api.createLibrary(lib);
      const rescans = !library || !!saved.job;
      invalidateLibraries(qc);
      if (rescans) noteScanStarted(qc, saved.id);
      toast.add({
        title: t(library ? 'libraries.toast.saved' : 'libraries.toast.added', { name: v.name }),
        description: rescans
          ? t(library ? 'libraries.toast.savedBody' : 'libraries.toast.addedBody')
          : undefined,
        type: 'success',
      });
      onDone();
    } catch (err) {
      const field =
        err instanceof ApiError && err.code === 'invalid_schedule'
          ? 'time'
          : err instanceof ApiError && err.code === 'invalid_pattern'
            ? 'ignore'
            : 'folder';
      form.setError(field, { message: errorMessage(err, t) });
    }
  });

  const folderValue = form.watch('folder');
  const choose = (path: string) => {
    form.setValue('folder', path, { shouldValidate: true, shouldDirty: true });
    const name = absoluteBaseName(path);
    if (!form.getValues('name').trim() && name) {
      form.setValue('name', name, { shouldValidate: true });
    }
  };

  const nameError = fieldMessage(errors.name?.message, t);
  const folderError = fieldMessage(errors.folder?.message, t);
  const timeError = fieldMessage(errors.time?.message, t);
  const ignoreError = fieldMessage(errors.ignore?.message, t);
  const schedule = form.watch('schedule');

  return (
    <form onSubmit={(e) => void onSubmit(e)} noValidate className="contents">
      <DialogBody className="flex flex-col gap-4">
        <Field htmlFor="library-name" label={t('libraries.form.name')} error={nameError}>
          <Input
            id="library-name"
            autoComplete="off"
            placeholder={t('libraries.form.namePlaceholder')}
            aria-invalid={nameError ? true : undefined}
            aria-describedby={describedBy('library-name', !!nameError, false)}
            {...form.register('name')}
          />
        </Field>
        <Field
          htmlFor="library-root"
          label={t('libraries.form.folder')}
          description={
            library ? t('libraries.form.folderEditHint') : t('libraries.form.folderHint')
          }
          error={folderError}
        >
          <div className="flex gap-2">
            <Input
              id="library-root"
              className="font-mono text-[13px]"
              autoComplete="off"
              spellCheck={false}
              placeholder="/mnt/audiobooks"
              aria-invalid={folderError ? true : undefined}
              aria-describedby={describedBy('library-root', !!folderError, true)}
              {...form.register('folder')}
            />
            <Button
              type="button"
              variant="outline"
              onClick={() => setBrowsing((b) => !b)}
              aria-expanded={browsing}
            >
              <FolderSearch aria-hidden="true" />
              {t('libraries.form.browse')}
            </Button>
          </div>
        </Field>
        {browsing ? (
          <ServerFolderPicker
            start={isAbsolutePath(folderValue.trim()) ? folderValue.trim() : ''}
            selected={folderValue.trim()}
            onChoose={choose}
            editingId={library?.id}
          />
        ) : null}
        <div className="grid gap-4 border-t pt-4 sm:grid-cols-2">
          <Field
            htmlFor="library-schedule"
            label={t('libraries.form.schedule')}
            description={t('libraries.form.scheduleHint')}
          >
            <NativeSelect
              id="library-schedule"
              aria-describedby="library-schedule-desc"
              {...form.register('schedule')}
            >
              {SCHEDULE_CHOICES.map((c) => (
                <option key={c} value={c}>
                  {c === 'daily' ? t('libraries.schedule.daily') : scheduleLabel(c, t)}
                </option>
              ))}
            </NativeSelect>
          </Field>
          {schedule === 'daily' ? (
            <Field
              htmlFor="library-time"
              label={t('libraries.form.time')}
              description={t('libraries.form.timeHint')}
              error={timeError}
            >
              <Input
                id="library-time"
                type="time"
                required
                aria-invalid={timeError ? true : undefined}
                aria-describedby={describedBy('library-time', !!timeError, true)}
                {...form.register('time')}
              />
            </Field>
          ) : null}
        </div>
        <Field
          htmlFor="library-ignore"
          label={t('libraries.form.ignore')}
          description={t('libraries.form.ignoreHint')}
          error={ignoreError}
        >
          <Textarea
            id="library-ignore"
            className="font-mono text-[13px]"
            rows={3}
            spellCheck={false}
            placeholder={'*.sample.mp3\nExtras/'}
            aria-invalid={ignoreError ? true : undefined}
            aria-describedby={describedBy('library-ignore', !!ignoreError, true)}
            {...form.register('ignore')}
          />
        </Field>
      </DialogBody>
      <DialogFormFooter
        busy={isSubmitting}
        submitLabel={library ? t('libraries.form.save') : t('libraries.form.submit')}
      />
    </form>
  );
}

/** The server's folders, one level at a time, starting at `start` ("" = the filesystem root). */
function ServerFolderPicker({
  start,
  selected,
  onChoose,
  editingId,
}: {
  start: string;
  selected: string;
  onChoose: (path: string) => void;
  editingId?: number;
}) {
  const { t } = useTranslation();
  const [path, setPath] = useState(start);
  const dirs = useDirs(path);
  const libraries = useLibraries();
  // Re-root at "/" when the typed folder (where the picker opened) can't be
  // listed, so it never dead-ends. A folder opened from the list that can't be
  // read keeps its error on screen, with the breadcrumb to step back.
  const failed = dirs.isError;
  useEffect(() => {
    if (failed && path !== '' && path === start) setPath('');
  }, [failed, path, start]);

  const inUse = new Map(
    (libraries.data ?? []).filter((l) => l.id !== editingId).map((l) => [l.root, l.name]),
  );
  const listing = dirs.data;
  const shown = listing?.path ?? path;
  return (
    <div className="flex flex-col gap-1.5">
      <FolderBrowser
        label={t('libraries.form.pickerLabel')}
        crumbs={shown ? absoluteCrumbs(shown) : []}
        onNavigate={setPath}
        loading={dirs.isPending}
        error={dirs.isError ? errorMessage(dirs.error, t) : undefined}
        emptyText={t('libraries.form.noSubfolders')}
        current={
          listing ? (
            <div className="flex items-center justify-between gap-3">
              <span className="min-w-0 truncate font-mono text-[12.5px]">{listing.path}</span>
              <Button
                type="button"
                size="sm"
                variant={selected === listing.path ? 'secondary' : 'default'}
                onClick={() => onChoose(listing.path)}
              >
                {selected === listing.path
                  ? t('libraries.form.chosen')
                  : t('libraries.form.useThis')}
              </Button>
            </div>
          ) : null
        }
        rows={(listing?.dirs ?? []).map((d) => ({
          path: d.path,
          name: d.name,
          kind: 'dir' as const,
          openable: true,
          detail: inUse.has(d.path)
            ? t('libraries.form.inUse', { name: inUse.get(d.path) })
            : undefined,
        }))}
        action={(row) => (
          <Button
            type="button"
            size="sm"
            variant={selected === row.path ? 'secondary' : 'outline'}
            onClick={() => onChoose(row.path)}
            aria-label={t('libraries.form.chooseAria', { name: row.name })}
          >
            {selected === row.path ? t('libraries.form.chosen') : t('libraries.form.choose')}
          </Button>
        )}
      />
      {listing?.truncated ? (
        <p className="text-[12px] text-muted-foreground">{t('libraries.form.truncated')}</p>
      ) : null}
    </div>
  );
}
