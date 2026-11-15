import { useId, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { ClipboardPaste, Lock } from 'lucide-react';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogBody,
  DialogClose,
  DialogContent,
  DialogFooter,
} from '@/components/ui/dialog';
import { Label } from '@/components/ui/label';
import { Textarea } from '@/components/ui/textarea';
import {
  type DraftRow,
  type FileSpan,
  formatStamp,
  parseTimestamps,
  rowsFromPaste,
  timelineEnd,
} from './chapters-edit-model';

/**
 * Paste timestamps: a chapter list as text (a time and a title a line, either
 * way round), previewed as the rows it makes (each file's start kept, fixed)
 * and the lines it couldn't use, before it replaces the editor's list.
 */
export function PasteTimestampsDialog({
  open,
  onOpenChange,
  spans,
  current,
  withHours,
  onReplace,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  spans: FileSpan[];
  current: DraftRow[];
  withHours: boolean;
  onReplace: (rows: DraftRow[]) => void;
}) {
  const { t } = useTranslation();
  const id = useId();
  const [text, setText] = useState('');
  const { parsed, rows } = useMemo(() => {
    const parsed = parseTimestamps(text, timelineEnd(spans));
    const rows = parsed.chapters.length ? rowsFromPaste(parsed.chapters, spans, current) : [];
    return { parsed, rows };
  }, [text, spans, current]);
  const close = (next: boolean) => {
    if (!next) setText('');
    onOpenChange(next);
  };
  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent
        size="lg"
        icon={ClipboardPaste}
        title={t('book.chapters.paste.title')}
        description={t('book.chapters.paste.description')}
      >
        <DialogBody className="flex flex-col gap-3">
          <Label htmlFor={id}>{t('book.chapters.paste.label')}</Label>
          <Textarea
            id={id}
            value={text}
            spellCheck={false}
            rows={6}
            placeholder={t('book.chapters.paste.placeholder')}
            onChange={(e) => setText(e.target.value)}
            className="font-mono text-[12.5px]"
          />
          {parsed.unreadable.length ? (
            <p className="text-[12.5px] text-warning">
              {t('book.chapters.paste.unreadable', { lines: parsed.unreadable.join(', ') })}
            </p>
          ) : null}
          {parsed.outside.length ? (
            <p className="text-[12.5px] text-warning">
              {t('book.chapters.paste.outside', { lines: parsed.outside.join(', ') })}
            </p>
          ) : null}
          <section aria-labelledby={`${id}-preview`} className="flex flex-col gap-1.5">
            <h4 id={`${id}-preview`} className="text-[12.5px] font-semibold">
              {rows.length
                ? t('book.chapters.paste.preview', { count: rows.length })
                : t('book.chapters.paste.empty')}
            </h4>
            {rows.length ? (
              <ol className="flex max-h-64 flex-col overflow-y-auto rounded-[10px] border text-[13px]">
                {rows.map((r, i) => (
                  <li
                    key={r.key}
                    className="grid grid-cols-[28px_96px_minmax(0,1fr)] items-center gap-2 border-b px-2.5 py-1 last:border-b-0"
                  >
                    <span className="text-[12px] text-subtle-foreground tabular-nums">{i + 1}</span>
                    <span className="flex items-center gap-1 text-muted-foreground tabular-nums">
                      {r.fixed ? (
                        <Lock
                          className="size-3 shrink-0"
                          aria-label={t('book.chapters.paste.fileStart')}
                        />
                      ) : null}
                      {formatStamp(r.start, withHours)}
                    </span>
                    <span className="truncate">{r.title}</span>
                  </li>
                ))}
              </ol>
            ) : null}
            {rows.length > 0 && spans.length > 1 ? (
              <p className="text-[12px] text-muted-foreground">
                {t('book.chapters.paste.fixedNote')}
              </p>
            ) : null}
          </section>
        </DialogBody>
        <DialogFooter>
          <DialogClose render={<Button type="button" variant="ghost" />}>
            {t('common.cancel')}
          </DialogClose>
          <Button
            disabled={!rows.length}
            onClick={() => {
              onReplace(rows);
              close(false);
            }}
          >
            {t('book.chapters.paste.replace')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
