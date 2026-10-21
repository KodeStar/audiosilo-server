import { useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { FileText, FolderTree, Info } from 'lucide-react';
import { api } from '@/api/client';
import { invalidateLibraries, invalidateLibraryBooks, keys, noteScanStarted } from '@/api/hooks';
import {
  METADATA_SOURCES,
  PREVIEW_FIELDS,
  type AdminLibrary,
  type LibraryPreview,
  type MetadataSource,
  type PreviewSample,
} from '@/api/types';
import { Notice } from '@/components/notice';
import { Badge } from '@/components/ui/badge';
import { Field } from '@/components/ui/field';
import { NativeSelect } from '@/components/ui/native-select';
import { RadioCards } from '@/components/ui/radio-cards';
import { errorMessage } from '@/lib/errors';
import { formatNumber } from '@/lib/format';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import { compareField, disagreements } from './setup-model';
import { StepFooter } from './step-parts';
import { previewQuery, type StepProps } from './step-actions';

/** What the naming step can name: the draft's new folder, or a library config declares. */
interface Target {
  key: string;
  root: string;
  name: string;
  /** Set for a library that exists already (a config-declared one). */
  library?: AdminLibrary;
}

/**
 * Step 2: how books are named. A sample of the folder's books as their tags
 * and their folders name them, side by side, with the blanks and the
 * disagreements marked and the side with fewer blanks preselected. The pick is
 * the new library's metadata_source; for a library config declares (already
 * scanned) another pick PATCHes it, which re-resolves its books.
 */
export function NamingStep({ env, libraries, draft, setDraft, leave, back, last }: StepProps) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const targets: Target[] = [
    ...(draft ? [{ key: 'draft', root: draft.root, name: draft.name }] : []),
    ...libraries
      .filter((l) => l.kind === 'books' && env.config_libraries.includes(l.name))
      .map((l) => ({ key: `library-${l.id}`, root: l.root, name: l.name, library: l })),
  ];
  const [targetKey, setTargetKey] = useState(targets[0]?.key);
  const target = targets.find((x) => x.key === targetKey) ?? targets[0];
  const preview = useQuery({ ...previewQuery(target?.root ?? ''), enabled: !!target });
  const [picked, setPicked] = useState<Partial<Record<string, MetadataSource>>>(() =>
    draft?.source ? { draft: draft.source } : {},
  );
  const choice: MetadataSource | undefined = target
    ? (picked[target.key] ?? preview.data?.suggested)
    : undefined;

  const onContinue = async () => {
    if (!target || !choice) return leave('skipped');
    const lib = target.library;
    if (!lib) {
      if (draft) setDraft({ ...draft, source: choice });
    } else if (choice !== lib.metadata_source) {
      const saved = await api.updateLibrary(lib.id, { metadata_source: choice });
      invalidateLibraries(qc);
      invalidateLibraryBooks(qc, lib.id);
      void qc.invalidateQueries({ queryKey: keys.issues });
      if (saved.job) noteScanStarted(qc, lib.id);
      toast.add({
        title: t('setup.naming.switched', { name: lib.name }),
        description: t(`setup.naming.switchedBody.${choice}`),
        type: 'success',
      });
    }
    await leave('done');
  };

  if (!target) {
    return (
      <>
        <Notice tone="info" icon={Info} title={t('setup.naming.none')}>
          {t('setup.naming.noneBody')}
        </Notice>
        <StepFooter
          back={back}
          onSkip={() => leave('skipped')}
          onContinue={() => leave('skipped')}
          last={last}
        />
      </>
    );
  }

  const lib = target.library;
  return (
    <>
      <div className="flex flex-col gap-5">
        {targets.length > 1 ? (
          <Field htmlFor="setup-naming-target" label={t('setup.naming.target')}>
            <NativeSelect
              id="setup-naming-target"
              value={target.key}
              onChange={(e) => setTargetKey(e.target.value)}
            >
              {targets.map((x) => (
                <option key={x.key} value={x.key}>
                  {x.library ? t('setup.naming.targetLibrary', { name: x.name }) : x.name}
                </option>
              ))}
            </NativeSelect>
          </Field>
        ) : null}

        {preview.isPending ? (
          <div
            className="skel h-[220px] rounded-xl"
            role="status"
            aria-label={t('common.loading')}
          />
        ) : preview.isError ? (
          <Notice tone="warn" icon={Info} role="status" title={t('setup.naming.previewFailed')}>
            {errorMessage(preview.error, t)}
          </Notice>
        ) : (
          <SampleTable preview={preview.data} choice={choice} lang={lang} />
        )}

        <RadioCards
          label={t('setup.naming.pick')}
          value={choice}
          onValueChange={(v) => setPicked((p) => ({ ...p, [target.key]: v }))}
          options={METADATA_SOURCES.map((s) => ({
            value: s,
            title: t(`libraries.source.${s}`),
            description: t(`setup.naming.pick.${s}`),
            aside:
              preview.data?.suggested === s ? (
                <Badge variant="brand">{t('setup.naming.suggested')}</Badge>
              ) : undefined,
          }))}
        />
        {lib && choice && choice !== lib.metadata_source ? (
          <p className="text-[13px] text-muted-foreground" role="status">
            {t('setup.naming.reresolve', {
              count: lib.book_count,
              formatted: formatNumber(lib.book_count, lang),
            })}
          </p>
        ) : null}
      </div>
      <StepFooter back={back} onSkip={() => leave('skipped')} onContinue={onContinue} last={last} />
    </>
  );
}

/** The samples side by side: each book's fields from tags and from folders. */
function SampleTable({
  preview: p,
  choice,
  lang,
}: {
  preview: LibraryPreview;
  choice: MetadataSource | undefined;
  lang: string;
}) {
  const { t } = useTranslation();
  if (p.samples.length === 0) {
    return <p className="text-muted-foreground">{t('setup.naming.noSamples')}</p>;
  }
  const differ = disagreements(p.samples);
  return (
    <div className="flex flex-col gap-3">
      <p className="text-[13px] text-muted-foreground">
        {t('setup.naming.summary', {
          tags: formatNumber(p.blanks.tags, lang),
          path: formatNumber(p.blanks.path, lang),
        })}{' '}
        {t('setup.naming.differ', { count: differ, formatted: formatNumber(differ, lang) })}
      </p>
      <ul className="flex flex-col gap-3" aria-label={t('setup.naming.samples')}>
        {p.samples.map((s) => (
          <Sample key={s.rel_path} sample={s} choice={choice} />
        ))}
      </ul>
    </div>
  );
}

function Sample({
  sample: s,
  choice,
}: {
  sample: PreviewSample;
  choice: MetadataSource | undefined;
}) {
  const { t } = useTranslation();
  const head = (side: MetadataSource, Icon: typeof FileText) => (
    <span
      className={cn(
        'inline-flex items-center gap-1.5 text-[12px] font-semibold',
        choice === side ? 'text-foreground' : 'text-muted-foreground',
      )}
    >
      <Icon className="size-[15px]" aria-hidden="true" />
      {t(`setup.naming.from.${side}`)}
    </span>
  );
  return (
    <li className="overflow-hidden rounded-xl border">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b bg-muted/40 px-3.5 py-2">
        <span className="min-w-0 font-mono text-[12.5px] [overflow-wrap:anywhere]">
          {s.rel_path}
        </span>
        {s.abs ? (
          <Badge variant="outline" title={t('setup.naming.absHint')}>
            {t('setup.naming.abs')}
          </Badge>
        ) : null}
      </div>
      <table className="w-full table-fixed text-[13px]">
        <thead>
          <tr className="border-b">
            <th scope="col" className="w-[84px] px-3.5 py-1.5 text-left sm:w-[110px]">
              <span className="text-[12px] font-semibold text-muted-foreground">
                {t('setup.naming.field')}
              </span>
            </th>
            <th scope="col" className="px-2 py-1.5 text-left">
              {head('tags', FileText)}
            </th>
            <th scope="col" className="px-2 py-1.5 text-left">
              {head('path', FolderTree)}
            </th>
          </tr>
        </thead>
        <tbody>
          {PREVIEW_FIELDS.map((f) => {
            const c = compareField(s, f);
            return (
              <tr
                key={f}
                className={cn(
                  'border-b last:border-b-0 align-top',
                  c.differs && 'bg-warning-soft/60',
                )}
              >
                <th
                  scope="row"
                  className="w-[84px] px-3.5 py-1.5 text-left text-[12px] font-semibold text-muted-foreground sm:w-[110px]"
                >
                  {t(`setup.naming.field.${f}`)}
                  {c.differs ? <span className="sr-only"> {t('setup.naming.differs')}</span> : null}
                </th>
                <Cell value={c.tags} />
                <Cell value={c.path} />
              </tr>
            );
          })}
        </tbody>
      </table>
    </li>
  );
}

function Cell({ value }: { value: string }) {
  const { t } = useTranslation();
  return (
    <td className="px-2 py-1.5 [overflow-wrap:anywhere]">
      {value ? (
        value
      ) : (
        <span className="text-subtle-foreground italic">{t('setup.naming.blank')}</span>
      )}
    </td>
  );
}
