import { useTranslation } from 'react-i18next';
import { Loader2, Merge } from 'lucide-react';
import type { MergeSuggestion, PersonCount, PersonField } from '@/api/types';
import { Notice } from '@/components/notice';
import { Button } from '@/components/ui/button';
import { counted } from '@/lib/format';
import { otherSpellingBooks, otherSpellings } from './people-model';
import { useMerge } from './use-merge';

/**
 * Spellings that look like one person, one notice each, with the merge that
 * rewrites the other spellings to the suggested one (Authors and Narrators).
 */
export function MergeSuggestions({
  field,
  suggestions,
  people,
  libraryId,
}: {
  field: PersonField;
  suggestions: MergeSuggestion[];
  people: PersonCount[];
  libraryId?: number;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const { merging, merge } = useMerge(field, libraryId);
  if (suggestions.length === 0) return null;

  const list = new Intl.ListFormat(lang, { type: 'conjunction' });
  return (
    <div className="mb-6 flex flex-col gap-3">
      {suggestions.map((s) => {
        const others = otherSpellings(s);
        const books = otherSpellingBooks(s, people);
        const busy = merging === s.suggested;
        return (
          <Notice
            key={s.names.join('\u0000')}
            tone="info"
            icon={Merge}
            title={t('people-merge.title', {
              count: others.length,
              names: list.format(others.map((name) => t('people-merge.quoted', { name }))),
              suggested: s.suggested,
            })}
            actions={
              <Button size="sm" disabled={merging !== null} onClick={() => void merge(s)}>
                {busy ? <Loader2 className="animate-spin" aria-hidden="true" /> : null}
                {busy
                  ? t('people-merge.merging')
                  : t(field === 'author' ? 'people-merge.authors' : 'people-merge.narrators')}
              </Button>
            }
          >
            {t('people-merge.body', counted(books, lang))}
          </Notice>
        );
      })}
    </div>
  );
}
