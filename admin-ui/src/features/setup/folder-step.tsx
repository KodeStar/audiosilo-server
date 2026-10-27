import { useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { FolderSearch, FolderOpen, LoaderCircle, TriangleAlert } from 'lucide-react';
import type { AdminLibrary, LibraryPreview } from '@/api/types';
import { Notice } from '@/components/notice';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Field } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { describedBy } from '@/lib/a11y';
import { errorMessage } from '@/lib/errors';
import { formatNumber } from '@/lib/format';
import { isAbsolutePath } from '@/lib/paths';
import { defaultLibraryName, folderErrorKey } from './setup-model';
import { StepFooter } from './step-parts';
import { StepStop, previewQuery, type StepProps } from './step-actions';

/**
 * Step 1: the books folder. A typed path (the server's mount points offered as
 * suggestions) checked by the preview: what it found, or why it can't be read,
 * in words that catch the Docker host-path mistake. The library is created
 * later, after its naming and region (decision 5); here it becomes the draft.
 */
export function FolderStep({ env, libraries, draft, setDraft, leave, back, last }: StepProps) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const [root, setRoot] = useState(draft?.root ?? '');
  const [name, setName] = useState<string | null>(draft?.name ?? null);
  // The folder last checked: its preview shows while the field still says it.
  const [checked, setChecked] = useState(draft?.root ?? '');
  const [inputError, setInputError] = useState<string | null>(null);
  const typed = root.trim();
  const preview = useQuery({ ...previewQuery(checked), enabled: checked !== '' });
  const current = checked !== '' && checked === typed;
  const shownName = name ?? defaultLibraryName(typed);
  // The folder checked and found: only then is there a library to name.
  const checkedOk = current && preview.data !== undefined;

  /**
   * Runs the preview for a folder; null when it was refused (the reason is
   * shown). Check asks again (a mount may have been fixed); Continue takes the
   * answer it already has.
   */
  const check = async (folder = typed, fresh = true): Promise<LibraryPreview | null> => {
    setInputError(null);
    if (!folder) {
      setInputError(t('setup.folder.required'));
      return null;
    }
    if (!isAbsolutePath(folder)) {
      setInputError(t('setup.folder.error.path_not_absolute'));
      return null;
    }
    setChecked(folder);
    try {
      return await qc.fetchQuery({ ...previewQuery(folder), ...(fresh ? { staleTime: 0 } : {}) });
    } catch {
      return null; // the query's error shows under the field
    }
  };

  const choose = (hint: string) => {
    setRoot(hint);
    void check(hint);
  };

  const onContinue = async () => {
    if (!typed) {
      // Nothing new to add: fine when the server already has a library.
      if (libraries.length === 0) {
        setInputError(t('setup.folder.requiredOrSkip'));
        throw new StepStop();
      }
      setDraft(null);
      return leave('done');
    }
    const found = await check(typed, false);
    if (!found) throw new StepStop();
    const sameDraft = draft?.root === typed;
    setDraft({
      root: typed,
      name: shownName.trim() || typed,
      ...(sameDraft && draft?.source ? { source: draft.source } : {}),
    });
    await leave('done');
  };

  const onSkip = async () => {
    setDraft(null);
    await leave('skipped');
  };

  const queryError = current && preview.isError ? preview.error : null;
  const errorKey = queryError ? folderErrorKey(queryError) : null;
  const folderError =
    inputError ??
    (queryError
      ? errorKey
        ? t(errorKey, { root: checked })
        : errorMessage(queryError, t)
      : undefined);

  return (
    <>
      <div className="flex flex-col gap-5">
        <Field
          htmlFor="setup-folder"
          label={t('setup.folder.label')}
          description={t('setup.folder.hint')}
          error={folderError}
        >
          <div className="flex flex-col gap-2 sm:flex-row">
            <Input
              id="setup-folder"
              className="font-mono text-[13px]"
              autoComplete="off"
              spellCheck={false}
              placeholder="/audiobooks"
              value={root}
              onChange={(e) => {
                setRoot(e.target.value);
                setInputError(null);
              }}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault();
                  void check();
                }
              }}
              aria-invalid={folderError ? true : undefined}
              aria-describedby={describedBy('setup-folder', !!folderError, true)}
            />
            <Button
              type="button"
              variant="outline"
              disabled={preview.isFetching}
              onClick={() => void check()}
            >
              {preview.isFetching ? (
                <LoaderCircle className="animate-spin" aria-hidden="true" />
              ) : (
                <FolderSearch aria-hidden="true" />
              )}
              {t('setup.folder.check')}
            </Button>
          </div>
        </Field>

        {env.mount_hints.length > 0 ? (
          <div className="flex flex-col gap-2">
            <span className="text-[13px] text-muted-foreground">{t('setup.folder.hints')}</span>
            <div className="flex flex-wrap gap-2">
              {env.mount_hints.map((h) => (
                <Button
                  key={h}
                  type="button"
                  variant="secondary"
                  size="sm"
                  className="font-mono"
                  aria-pressed={typed === h}
                  onClick={() => choose(h)}
                >
                  <FolderOpen aria-hidden="true" />
                  {h}
                </Button>
              ))}
            </div>
          </div>
        ) : null}

        {current && preview.data ? <PreviewFound preview={preview.data} lang={lang} /> : null}

        {checkedOk ? (
          <Field
            htmlFor="setup-library-name"
            label={t('setup.folder.name')}
            description={t('setup.folder.nameHint')}
          >
            <Input
              id="setup-library-name"
              autoComplete="off"
              value={shownName}
              onChange={(e) => setName(e.target.value)}
              aria-describedby="setup-library-name-desc"
            />
          </Field>
        ) : null}

        {libraries.length > 0 ? (
          <AlreadyAdded libraries={libraries} configNames={env.config_libraries} />
        ) : null}
      </div>
      <StepFooter back={back} onSkip={onSkip} onContinue={onContinue} last={last} />
    </>
  );
}

/** What the check found: the counts, and a warning when nothing or a library's folder was found. */
function PreviewFound({ preview: p, lang }: { preview: LibraryPreview; lang: string }) {
  const { t } = useTranslation();
  const n = (v: number) => formatNumber(v, lang);
  if (p.counts.audio_files === 0) {
    return (
      <Notice tone="warn" icon={TriangleAlert} role="status" title={t('setup.folder.empty')}>
        {t('setup.folder.emptyBody')}
      </Notice>
    );
  }
  return (
    <div className="flex flex-col gap-3">
      <div role="status" className="rounded-xl border bg-muted/40 px-4 py-3.5">
        <b className="font-semibold">
          {t('setup.folder.found', {
            count: p.counts.audio_files,
            formatted: n(p.counts.audio_files),
          })}
        </b>
        <p className="text-[13px] text-muted-foreground">
          {t('setup.folder.books', {
            folders: n(p.counts.folder_books),
            files: n(p.counts.file_books),
          })}
          {p.counts.capped ? ` ${t('setup.folder.capped')}` : ''}
        </p>
      </div>
      {p.same_root ? (
        <Notice
          tone="warn"
          icon={TriangleAlert}
          title={t('setup.folder.sameRoot', { name: p.same_root })}
        >
          {t('setup.folder.sameRootBody')}
        </Notice>
      ) : null}
    </div>
  );
}

/** The libraries the server already has, config-declared ones marked as such. */
function AlreadyAdded({
  libraries,
  configNames,
}: {
  libraries: AdminLibrary[];
  configNames: string[];
}) {
  const { t } = useTranslation();
  return (
    <section aria-labelledby="setup-added" className="flex flex-col gap-2">
      <h3 id="setup-added" className="text-[13px] font-semibold">
        {t('setup.folder.added')}
      </h3>
      <ul className="divide-y rounded-xl border">
        {libraries.map((l) => (
          <li key={l.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-3">
            <div className="flex min-w-0 flex-1 basis-48 flex-col">
              <span className="font-semibold">{l.name}</span>
              <span className="font-mono text-[12.5px] text-muted-foreground [overflow-wrap:anywhere]">
                {l.root}
              </span>
            </div>
            <div className="flex flex-wrap gap-1.5">
              <Badge variant="secondary">{t(`libraries.kind.${l.kind}`)}</Badge>
              {configNames.includes(l.name) ? (
                <Badge variant="outline">{t('setup.folder.fromConfig')}</Badge>
              ) : null}
            </div>
          </li>
        ))}
      </ul>
    </section>
  );
}
