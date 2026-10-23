import { useEffect, useState } from 'react';
import { Controller, useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { z } from '@/lib/zod';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Database, FolderSearch } from 'lucide-react';
import { ApiError, api } from '@/api/client';
import {
  invalidateAfterSourceChange,
  invalidateLibraries,
  keys,
  noteScanStarted,
  useDirs,
  useLibraries,
  useModules,
  useSetup,
} from '@/api/hooks';
import {
  METADATA_SOURCES,
  type AdminLibrary,
  type AdminUser,
  type Job,
  type Library,
} from '@/api/types';
import { FolderBrowser } from '@/components/folder-browser';
import { Button } from '@/components/ui/button';
import { Checkbox } from '@/components/ui/checkbox';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { Field } from '@/components/ui/field';
import { describedBy } from '@/lib/a11y';
import { Input } from '@/components/ui/input';
import { NativeSelect } from '@/components/ui/native-select';
import { Textarea } from '@/components/ui/textarea';
import { errorMessage, fieldMessage } from '@/lib/errors';
import { absoluteBaseName, absoluteCrumbs, isAbsolutePath } from '@/lib/paths';
import { toast } from '@/lib/toast';
import { configLibrary, everyoneToGrant, podcastsHintKey } from './library-access';
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
  kind: z.enum(['books', 'podcasts']),
  name: z.string().trim().min(1, 'libraries.form.nameRequired'),
  // Not `root`: react-hook-form reserves errors.root for form-level errors.
  folder: z.string().trim().min(1, 'libraries.form.folderRequired'),
  schedule: z.enum(SCHEDULE_CHOICES as [ScheduleChoice, ...ScheduleChoice[]]),
  time: z.string(),
  ignore: z.string(),
  source: z.enum(METADATA_SOURCES),
  // A new podcast library only: grant it to every listener (BRIDGE-07).
  everyone: z.boolean(),
});
type Values = z.infer<typeof schema>;

/** The field a refused save's error code belongs under (anything else: the folder). */
const FIELD_BY_CODE: Partial<Record<string, keyof Values>> = {
  root_overlaps: 'folder',
  invalid_schedule: 'time',
  invalid_pattern: 'ignore',
  invalid_metadata_source: 'source',
  config_library: 'name',
};

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
  // A library config.yaml declares keeps its name and folder there (the server
  // refuses changing them, 409 config_library): shown, not editable.
  const setup = useSetup().data;
  const fromConfig = !!library && configLibrary(setup, library.name);
  const current = splitSchedule(library?.scan_schedule ?? '');
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: {
      kind: library?.kind ?? 'books',
      name: library?.name ?? '',
      folder: library?.root ?? '',
      schedule: current.choice,
      time: current.time,
      ignore: patternsToText(library?.ignore_patterns ?? []),
      source: library?.metadata_source ?? 'tags',
      everyone: true,
    },
  });
  const { errors, isSubmitting } = form.formState;

  /** Grants a new library to every listener: how many grants failed, or none known (the people list didn't load). */
  const grantEveryone = async (libraryId: number): Promise<{ failed: number | null }> => {
    let users: AdminUser[];
    try {
      users = await qc.fetchQuery({
        queryKey: keys.users,
        queryFn: () => api.users().then((r) => r.users ?? []),
      });
    } catch {
      return { failed: null };
    }
    const results = await Promise.allSettled(
      everyoneToGrant(users).map((u) => api.grantLibrary(u.id, libraryId)),
    );
    return { failed: results.filter((r) => r.status === 'rejected').length };
  };

  const onSubmit = form.handleSubmit(async (v) => {
    const patterns = textToPatterns(v.ignore);
    const lib = {
      kind: v.kind,
      name: v.name,
      root: v.folder,
      scan_schedule: joinSchedule(v.schedule, v.time),
      ...(v.kind === 'books' ? { ignore_patterns: patterns, metadata_source: v.source } : {}),
    };
    try {
      // A new library is always scanned; an edit only when the server queued a
      // scan for it (a new folder or new ignore rules).
      const saved: Library & { job?: Job } = library
        ? await api.updateLibrary(library.id, lib)
        : await api.createLibrary(lib);
      const rescans = !library || !!saved.job;
      // A new podcast library is no one's until access is given: "Give
      // everyone access" grants it to each listener now. A refusal says so;
      // the library stays added.
      const granted =
        !library && v.kind === 'podcasts' && v.everyone ? await grantEveryone(saved.id) : undefined;
      invalidateLibraries(qc);
      if (library && v.source !== library.metadata_source) {
        invalidateAfterSourceChange(qc, library.id);
      }
      if (rescans) noteScanStarted(qc, saved.id);
      if (granted) {
        void qc.invalidateQueries({ queryKey: keys.users });
        if (granted.failed !== 0) {
          toast.add({
            title:
              granted.failed === null
                ? t('libraries.toast.accessFailedAll')
                : t('libraries.toast.accessFailed', { count: granted.failed }),
            description: t('libraries.toast.accessFailedBody'),
            type: 'error',
          });
        }
      }
      toast.add({
        title: t(library ? 'libraries.toast.saved' : 'libraries.toast.added', { name: v.name }),
        description: rescans
          ? t(
              library
                ? 'libraries.toast.savedBody'
                : v.kind === 'podcasts'
                  ? 'libraries.toast.podcastAddedBody'
                  : 'libraries.toast.addedBody',
            )
          : undefined,
        type: 'success',
      });
      onDone();
    } catch (err) {
      const field = (err instanceof ApiError && err.code && FIELD_BY_CODE[err.code]) || 'folder';
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
  const sourceError = fieldMessage(errors.source?.message, t);
  const schedule = form.watch('schedule');
  const isPodcasts = form.watch('kind') === 'podcasts';
  const podcastsModule = useModules().data?.modules.find((m) => m.id === 'podcasts');

  return (
    <form onSubmit={(e) => void onSubmit(e)} noValidate className="contents">
      <DialogBody className="flex flex-col gap-4">
        <Field
          htmlFor="library-kind"
          label={t('libraries.form.kind')}
          description={t(podcastsHintKey(podcastsModule))}
        >
          <NativeSelect id="library-kind" disabled={!!library} {...form.register('kind')}>
            <option value="books">{t('libraries.kind.books')}</option>
            <option value="podcasts">{t('libraries.kind.podcasts')}</option>
          </NativeSelect>
        </Field>
        {fromConfig ? (
          <p className="text-[13px] text-muted-foreground" role="note">
            {t('libraries.form.configLocked')}
          </p>
        ) : null}
        <Field htmlFor="library-name" label={t('libraries.form.name')} error={nameError}>
          <Input
            id="library-name"
            readOnly={fromConfig}
            autoComplete="off"
            placeholder={t(
              isPodcasts
                ? 'libraries.form.podcastNamePlaceholder'
                : 'libraries.form.namePlaceholder',
            )}
            aria-invalid={nameError ? true : undefined}
            aria-describedby={describedBy('library-name', !!nameError, false)}
            {...form.register('name')}
          />
        </Field>
        <Field
          htmlFor="library-root"
          label={t('libraries.form.folder')}
          description={
            isPodcasts
              ? t(
                  library
                    ? 'libraries.form.podcastFolderEditHint'
                    : 'libraries.form.podcastFolderHint',
                )
              : library
                ? t('libraries.form.folderEditHint')
                : t('libraries.form.folderHint')
          }
          error={folderError}
        >
          <div className="flex gap-2">
            <Input
              id="library-root"
              readOnly={fromConfig}
              className="font-mono text-[13px]"
              autoComplete="off"
              spellCheck={false}
              placeholder={isPodcasts ? '/mnt/podcasts' : '/mnt/audiobooks'}
              aria-invalid={folderError ? true : undefined}
              aria-describedby={describedBy('library-root', !!folderError, true)}
              {...form.register('folder')}
            />
            <Button
              type="button"
              variant="outline"
              disabled={fromConfig}
              onClick={() => setBrowsing((b) => !b)}
              aria-expanded={browsing}
            >
              <FolderSearch aria-hidden="true" />
              {t('libraries.form.browse')}
            </Button>
          </div>
        </Field>
        {browsing && !fromConfig ? (
          <ServerFolderPicker
            start={isAbsolutePath(folderValue.trim()) ? folderValue.trim() : ''}
            selected={folderValue.trim()}
            onChoose={choose}
            editingId={library?.id}
          />
        ) : null}
        <div className="grid grid-cols-1 gap-4 border-t pt-4 sm:grid-cols-2">
          <Field
            htmlFor="library-schedule"
            label={t('libraries.form.schedule')}
            description={t(
              isPodcasts ? 'libraries.form.podcastScheduleHint' : 'libraries.form.scheduleHint',
            )}
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
        {isPodcasts && !library ? (
          <div className="flex flex-col gap-1 border-t pt-4">
            <label className="flex items-start gap-2 text-[13.5px] font-semibold">
              <Controller
                control={form.control}
                name="everyone"
                render={({ field }) => (
                  <Checkbox
                    className="mt-0.5"
                    checked={field.value}
                    onCheckedChange={(c) => field.onChange(Boolean(c))}
                    aria-describedby="library-everyone-desc"
                  />
                )}
              />
              {t('libraries.form.everyone')}
            </label>
            <p id="library-everyone-desc" className="ml-6 text-[12.5px] text-muted-foreground">
              {t('libraries.form.everyoneHint')}
            </p>
          </div>
        ) : null}
        {!isPodcasts ? (
          <>
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
            <Field
              htmlFor="library-source"
              label={t('libraries.form.source')}
              description={t('libraries.form.sourceHint')}
              error={sourceError}
            >
              <NativeSelect
                id="library-source"
                aria-invalid={sourceError ? true : undefined}
                aria-describedby={describedBy('library-source', !!sourceError, true)}
                {...form.register('source')}
              >
                {METADATA_SOURCES.map((s) => (
                  <option key={s} value={s}>
                    {t(`libraries.source.${s}`)}
                  </option>
                ))}
              </NativeSelect>
            </Field>
          </>
        ) : null}
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
