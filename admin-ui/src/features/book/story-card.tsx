import { useId, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { ChevronDown, ChevronRight, ExternalLink, Send, Sparkles } from 'lucide-react';
import { useBookMeta, useServerInfo } from '@/api/hooks';
import type { AdminBookDetail, MetaAttribution, MetaWork } from '@/api/types';
import { ProvenanceMarker } from '@/components/provenance';
import { QueryError } from '@/components/query-error';
import { Badge } from '@/components/ui/badge';
import { Card, CardHeader } from '@/components/ui/card';
import { Tabs, TabsList, TabsPanel, TabsTab } from '@/components/ui/tabs';
import { safeHttpUrl } from '@/features/bridge/bridge-model';
import { contributedLinks, generateJobsKnown } from '@/features/bridge/generate-model';
import { bookJobsQuery } from '@/features/bridge/use-generate';
import { formatDate, formatList } from '@/lib/format';
import { FIRST_ROWS, ShowAllToggle } from './show-all-toggle';
import {
  recapHeading,
  recapRows,
  roleKey,
  STORY_CARD_ID,
  storySources,
  summaryParts,
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
 * /meta is keyed by the book's ASIN and ISBN, so a rematch asks again.
 */
export function StoryCard({ detail }: { detail: AdminBookDetail }) {
  const { t } = useTranslation();
  const server = useServerInfo().data;
  const b = detail.book;
  const on = Boolean(server?.capabilities.metadata);
  const meta = useBookMeta(b, on);
  // The jobs list only: GenerateSection follows the job and refreshes the page when it ends.
  const jobs = useQuery(bookJobsQuery(b.library_id, b.path, generateJobsKnown(server))).data?.jobs;
  if (!on) return null;

  const work = meta.data?.work;
  const sources = storySources(work, contributedLinks(jobs));
  const characters = work?.characters ?? [];
  const recaps = recapRows(work);
  const summary = summaryParts(work);

  return (
    <Card id={STORY_CARD_ID} tabIndex={-1} className="outline-none" aria-labelledby="story-title">
      <CardHeader titleId="story-title" title={t('book.story.title')} />
      {meta.isPending ? (
        <div className="flex flex-col gap-2 p-5" role="status" aria-label={t('common.loading')}>
          <span className="skel h-4 w-40" />
          <span className="skel h-4 w-56" />
        </div>
      ) : meta.isError ? (
        <div className="p-5">
          <QueryError
            title={t('book.story.error')}
            error={meta.error}
            onRetry={() => void meta.refetch()}
          />
        </div>
      ) : (
        <Tabs defaultValue="characters">
          <TabsList className="px-5" aria-label={t('book.story.title')}>
            <TabsTab value="characters" count={characters.length}>
              {t('book.story.characters')}
            </TabsTab>
            {/* The whole-book summary is a row of the list too. */}
            <TabsTab value="recaps" count={recaps.length + (summary ? 1 : 0)}>
              {t('book.story.recaps')}
            </TabsTab>
          </TabsList>
          <TabsPanel value="characters">
            <SourceLine dimension={sources.characters} attribution={work?.attribution} />
            {sources.characters.source !== 'none' ? (
              <CharacterList characters={characters} />
            ) : (
              <Empty text={t('book.story.noCharacters')} />
            )}
          </TabsPanel>
          <TabsPanel value="recaps">
            <SourceLine dimension={sources.recaps} attribution={work?.attribution} />
            {sources.recaps.source !== 'none' ? (
              <RecapList recaps={recaps} summary={summary} />
            ) : (
              <Empty text={t('book.story.noRecaps')} />
            )}
          </TabsPanel>
        </Tabs>
      )}
    </Card>
  );
}

function Empty({ text }: { text: string }) {
  return <p className="px-5 pb-5 text-[13px] text-muted-foreground">{text}</p>;
}

/** An external link when there is a safe one, else the plain text. */
function MaybeLink({ href, children }: { href?: string; children: string }) {
  return href ? (
    <a href={href} target="_blank" rel="noopener noreferrer" className="hover:underline">
      {children}
    </a>
  ) : (
    children
  );
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
  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5 px-5 pt-4 pb-3 text-[12px] text-muted-foreground">
      {dimension.source === 'community' ? (
        <>
          <ProvenanceMarker source="community" />
          {attribution ? (
            <span className="[overflow-wrap:anywhere]">
              <MaybeLink href={safeHttpUrl(attribution.source_url)}>{attribution.credit}</MaybeLink>
              {' · '}
              <MaybeLink href={safeHttpUrl(attribution.license_url)}>
                {attribution.license}
              </MaybeLink>
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

function CharacterList({ characters }: { characters: NonNullable<MetaWork['characters']> }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const [all, setAll] = useState(false);
  const shown = all ? characters : characters.slice(0, FIRST_ROWS);
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
      <ShowAllToggle
        className="border-t px-3 py-2"
        all={all}
        total={characters.length}
        label={t('book.story.allCharacters', { count: characters.length })}
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
  summary?: { inShort?: string; ending?: string };
}) {
  const { t } = useTranslation();
  const [all, setAll] = useState(false);
  const shown = all ? recaps : recaps.slice(0, FIRST_ROWS);
  return (
    <>
      <p className="px-5 pb-3 text-[12.5px] text-muted-foreground">{t('book.story.recapsHint')}</p>
      <ul className="divide-y border-t">
        {shown.map((r, i) => {
          const heading = recapHeading(r);
          return (
            <li key={`${r.through.chapter}:${i}`}>
              <Reveal label={t(heading.key, { n: heading.n })}>
                <p className="text-[13px] whitespace-pre-line">{r.text}</p>
              </Reveal>
            </li>
          );
        })}
        {summary ? (
          <li>
            <Reveal label={t('book.story.summary')}>
              <SummaryPart label={t('book.story.inShort')} text={summary.inShort} />
              <SummaryPart label={t('book.story.ending')} text={summary.ending} />
            </Reveal>
          </li>
        ) : null}
      </ul>
      <ShowAllToggle
        className="border-t px-3 py-2"
        all={all}
        total={recaps.length}
        label={t('book.story.allRecaps', { count: recaps.length })}
        onToggle={() => setAll(!all)}
      />
    </>
  );
}

function SummaryPart({ label, text }: { label: string; text?: string }) {
  if (!text) return null;
  return (
    <div className="flex flex-col gap-0.5">
      <h5 className="text-[12px] font-semibold text-muted-foreground">{label}</h5>
      <p className="text-[13px] whitespace-pre-line">{text}</p>
    </div>
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
