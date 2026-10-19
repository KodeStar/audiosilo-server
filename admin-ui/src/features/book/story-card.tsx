import { useEffect, useId, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { ChevronDown, ChevronRight, ExternalLink, Globe, Send, Sparkles } from 'lucide-react';
import { keys, useBookMeta, useServerInfo } from '@/api/hooks';
import type { AdminBookDetail, MetaAttribution, MetaWork } from '@/api/types';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { Tabs, TabsList, TabsPanel, TabsTab } from '@/components/ui/tabs';
import { safeHttpUrl } from '@/features/bridge/bridge-model';
import { contributedLinks, generateJobsKnown } from '@/features/bridge/generate-model';
import { useBookGenerateJob } from '@/features/bridge/use-generate';
import { formatDate, formatList } from '@/lib/format';
import {
  hasSummary,
  recapRows,
  roleKey,
  STORY_CARD_ID,
  STORY_FIRST_ROWS,
  storySources,
  type StoryDimension,
  type StoryRecap,
} from './story-model';

/**
 * The book's characters and recaps as /meta serves them (BRIDGE-PLAN.md Phase
 * 1e), read-only: a tab each, with where the entries came from (the community,
 * with its credit; generated on this server, with when; and a contribution
 * still waiting for review, with its issue). Recaps stay closed until asked
 * for, one chapter at a time, since the admin may be mid-book. Shown while
 * community metadata is on (the generated content is served through /meta too).
 */
export function StoryCard({ detail }: { detail: AdminBookDetail }) {
  const { t } = useTranslation();
  const server = useServerInfo().data;
  const b = detail.book;
  const on = Boolean(server?.capabilities.metadata);
  const meta = useBookMeta(b.library_id, b.path, on);
  const { jobs } = useBookGenerateJob(b.library_id, b.path, generateJobsKnown(server));
  useMetaAfterRematch(detail);
  if (!on) return null;

  const work = meta.data?.work;
  const sources = storySources(work, contributedLinks(jobs));
  const characters = work?.characters ?? [];
  const recaps = recapRows(work);

  return (
    <Card id={STORY_CARD_ID} tabIndex={-1} className="outline-none" aria-labelledby="story-title">
      <CardHeader titleId="story-title" title={t('book.story.title')} />
      {meta.isPending ? (
        <div className="flex flex-col gap-2 p-5" role="status" aria-label={t('common.loading')}>
          <span className="skel h-4 w-40" />
          <span className="skel h-4 w-56" />
        </div>
      ) : meta.isError ? (
        <div className="flex flex-wrap items-center justify-between gap-2 p-5">
          <p className="text-[13px] text-muted-foreground">{t('book.story.error')}</p>
          <Button variant="outline" size="sm" onClick={() => void meta.refetch()}>
            {t('common.tryAgain')}
          </Button>
        </div>
      ) : (
        <Tabs defaultValue="characters">
          <TabsList className="px-5" aria-label={t('book.story.title')}>
            <TabsTab value="characters" count={characters.length}>
              {t('book.story.characters')}
            </TabsTab>
            <TabsTab value="recaps" count={recaps.length}>
              {t('book.story.recaps')}
            </TabsTab>
          </TabsList>
          <TabsPanel value="characters">
            <SourceLine dimension={sources.characters} attribution={work?.attribution} />
            {characters.length ? (
              <CharacterList characters={characters} />
            ) : (
              <p className="px-5 pb-5 text-[13px] text-muted-foreground">
                {t('book.story.noCharacters')}
              </p>
            )}
          </TabsPanel>
          <TabsPanel value="recaps">
            <SourceLine dimension={sources.recaps} attribution={work?.attribution} />
            {recaps.length || hasSummary(work) ? (
              <RecapList recaps={recaps} summary={work?.recap_summary} />
            ) : (
              <p className="px-5 pb-5 text-[13px] text-muted-foreground">
                {t('book.story.noRecaps')}
              </p>
            )}
          </TabsPanel>
        </Tabs>
      )}
    </Card>
  );
}

/**
 * Asks /meta again when the book's match changes (a new ASIN or ISBN is a new
 * community work), rather than showing the old work's entries for the hour the
 * answer is kept.
 */
function useMetaAfterRematch(detail: AdminBookDetail) {
  const qc = useQueryClient();
  const { library_id, path, matched } = detail.book;
  const ids = `${detail.fields.asin.value}|${detail.fields.isbn.value}|${matched}`;
  const seen = useRef(ids);
  useEffect(() => {
    if (seen.current === ids) return;
    seen.current = ids;
    void qc.invalidateQueries({ queryKey: keys.bookMeta(library_id, path) });
  }, [ids, qc, library_id, path]);
}

/** Where a dimension's entries came from, as badges, above its list. */
function SourceLine({
  dimension,
  attribution,
}: {
  dimension: StoryDimension;
  attribution?: MetaAttribution;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  if (dimension.source === 'none') return <div className="pt-4" />;
  const sourceUrl = safeHttpUrl(attribution?.source_url);
  const licenseUrl = safeHttpUrl(attribution?.license_url);
  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5 px-5 pt-4 pb-3 text-[12px] text-muted-foreground">
      {dimension.source === 'community' ? (
        <>
          <Badge className="bg-prov-community-soft text-prov-community">
            <Globe aria-hidden="true" />
            {t('book.story.source.community')}
          </Badge>
          {attribution ? (
            <span className="[overflow-wrap:anywhere]">
              {sourceUrl ? (
                <a
                  href={sourceUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="hover:underline"
                >
                  {attribution.credit}
                </a>
              ) : (
                attribution.credit
              )}
              {' · '}
              {licenseUrl ? (
                <a
                  href={licenseUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="hover:underline"
                >
                  {attribution.license}
                </a>
              ) : (
                attribution.license
              )}
            </span>
          ) : null}
        </>
      ) : (
        <>
          <Badge variant="secondary">
            <Sparkles aria-hidden="true" />
            {t('book.story.source.generated')}
          </Badge>
          {dimension.generatedAt ? (
            <span className="tabular-nums">{formatDate(dimension.generatedAt, lang)}</span>
          ) : null}
          {dimension.contributed ? (
            <a
              href={dimension.contributed}
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex items-center gap-1 font-semibold text-brand-ink hover:underline"
            >
              <Send className="size-3.5" aria-hidden="true" />
              {t('book.story.contributed')}
              <ExternalLink className="size-3.5" aria-hidden="true" />
            </a>
          ) : null}
        </>
      )}
    </div>
  );
}

/** "Show all N" / "Show fewer" under a list cut at STORY_FIRST_ROWS. */
function MoreToggle({
  all,
  total,
  allKey,
  onToggle,
}: {
  all: boolean;
  total: number;
  allKey: string;
  onToggle: () => void;
}) {
  const { t } = useTranslation();
  if (total <= STORY_FIRST_ROWS) return null;
  return (
    <div className="border-t px-3 py-2">
      <Button variant="ghost" size="sm" onClick={onToggle}>
        {all ? t('book.chapters.fewer') : t(allKey, { count: total })}
      </Button>
    </div>
  );
}

function CharacterList({ characters }: { characters: NonNullable<MetaWork['characters']> }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const [all, setAll] = useState(false);
  const shown = all ? characters : characters.slice(0, STORY_FIRST_ROWS);
  return (
    <>
      <ul className="divide-y border-t">
        {shown.map((c, i) => {
          const role = roleKey(c.role);
          return (
            <li key={`${c.id}:${i}`} className="flex flex-col gap-1 px-5 py-3">
              <div className="flex flex-wrap items-baseline gap-x-2">
                <b className="font-semibold">{c.name}</b>
                {c.role ? (
                  <span className="text-[12px] text-muted-foreground">
                    {role ? t(role) : c.role}
                  </span>
                ) : null}
              </div>
              {c.aliases?.length ? (
                <p className="text-[12px] text-muted-foreground">
                  {t('book.story.aliases', { names: formatList(c.aliases, lang) })}
                </p>
              ) : null}
              {c.description ? (
                <p className="text-[13px] whitespace-pre-line">{c.description}</p>
              ) : null}
              <p className="text-[12px] text-subtle-foreground tabular-nums">
                {c.reveal.chapter > 0
                  ? t('book.story.fromChapter', { n: c.reveal.chapter })
                  : t('book.story.fromStart')}
              </p>
            </li>
          );
        })}
      </ul>
      <MoreToggle
        all={all}
        total={characters.length}
        allKey="book.story.allCharacters"
        onToggle={() => setAll(!all)}
      />
    </>
  );
}

function RecapList({
  recaps,
  summary,
}: {
  recaps: StoryRecap[];
  summary?: MetaWork['recap_summary'];
}) {
  const { t } = useTranslation();
  const [all, setAll] = useState(false);
  const shown = all ? recaps : recaps.slice(0, STORY_FIRST_ROWS);
  const inShort = summary?.in_short?.trim();
  const ending = summary?.ending?.trim();
  return (
    <>
      <p className="px-5 pb-3 text-[12.5px] text-muted-foreground">{t('book.story.recapsHint')}</p>
      <ul className="divide-y border-t">
        {shown.map((r, i) => (
          <li key={`${r.through.chapter}:${i}`}>
            <Reveal
              label={
                r.through.chapter > 0
                  ? t('book.story.through', { n: r.through.chapter })
                  : t('book.story.previously')
              }
            >
              <p className="text-[13px] whitespace-pre-line">{r.text}</p>
            </Reveal>
          </li>
        ))}
        {inShort || ending ? (
          <li>
            <Reveal label={t('book.story.summary')}>
              {inShort ? (
                <div className="flex flex-col gap-0.5">
                  <h5 className="text-[12px] font-semibold text-muted-foreground">
                    {t('book.story.inShort')}
                  </h5>
                  <p className="text-[13px] whitespace-pre-line">{inShort}</p>
                </div>
              ) : null}
              {ending ? (
                <div className="flex flex-col gap-0.5">
                  <h5 className="text-[12px] font-semibold text-muted-foreground">
                    {t('book.story.ending')}
                  </h5>
                  <p className="text-[13px] whitespace-pre-line">{ending}</p>
                </div>
              ) : null}
            </Reveal>
          </li>
        ) : null}
      </ul>
      <MoreToggle
        all={all}
        total={recaps.length}
        allKey="book.story.allRecaps"
        onToggle={() => setAll(!all)}
      />
    </>
  );
}

/** A row whose text stays hidden until its button is pressed (and hides again). */
function Reveal({ label, children }: { label: string; children: React.ReactNode }) {
  const [open, setOpen] = useState(false);
  const id = useId();
  const Chevron = open ? ChevronDown : ChevronRight;
  return (
    <>
      <button
        type="button"
        className="flex w-full cursor-pointer items-center gap-2 px-5 py-2.5 text-left text-[13px] font-[550] outline-none hover:bg-muted/70 focus-visible:bg-muted/70"
        aria-expanded={open}
        aria-controls={open ? id : undefined}
        onClick={() => setOpen(!open)}
      >
        <Chevron className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
        {label}
      </button>
      {open ? (
        <div id={id} className="flex flex-col gap-2 px-5 pb-3 pl-11">
          {children}
        </div>
      ) : null}
    </>
  );
}
