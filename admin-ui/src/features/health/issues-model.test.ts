import { adminBook } from '@/test/library-fixtures';
import { issuesSummary } from '@/test/fixtures';
import { attentionTotal, chaptersCheckNote, issueReason, pickCategory } from './issues-model';

it('says why a book is listed, per category', () => {
  const broken = adminBook({
    scan_error: 'empty_file',
    scan_error_file: 'Author/Book/07.mp3',
    scan_error_detail: '',
  });
  expect(issueReason('scan_error', broken)).toEqual({
    key: 'health.reason.empty_file',
    values: { file: '07.mp3', detail: '' },
  });
  expect(issueReason('suspect', adminBook({ suspect_parts: 3, file_count: 3 }))).toEqual({
    key: 'health.reason.suspect',
    values: { count: 3, files: 3 },
  });
  // A split book is listed by its first disc and named by the folder holding them.
  expect(issueReason('split_discs', adminBook({ path: 'Cowell/Dragonese/CD 1' }))).toEqual({
    key: 'health.reason.split_discs',
    values: { folder: 'Dragonese' },
  });
  // One chapter reads differently from none; hours round down.
  expect(
    issueReason('no_chapters', adminBook({ chapter_count: 1, duration: 3 * 3600 + 1800 })),
  ).toEqual({
    key: 'health.reason.one_chapter',
    values: { hours: 3 },
  });
  expect(issueReason('no_chapters', adminBook({ chapter_count: 0, duration: 7300 })).key).toBe(
    'health.reason.no_chapters',
  );
  expect(issueReason('transcode', adminBook({ codec: 'ac3' })).values).toEqual({ codec: 'AC3' });
  expect(issueReason('detailed_chapters', adminBook({ chapter_count: 34 }))).toEqual({
    key: 'health.reason.detailed_chapters',
    values: { count: 34 },
  });
});

it('says why a book without chapters got none from the community', () => {
  expect(chaptersCheckNote('no_chapters', adminBook({ chapters_check: 'crosses_files' }))).toBe(
    'health.chaptersCheck.crosses_files',
  );
  // Not checked, or a fit (it would have chapters now): nothing to say.
  expect(chaptersCheckNote('no_chapters', adminBook())).toBeUndefined();
  expect(chaptersCheckNote('no_chapters', adminBook({ chapters_check: 'fill' }))).toBeUndefined();
  expect(chaptersCheckNote('no_cover', adminBook({ chapters_check: 'no_match' }))).toBeUndefined();
});

it('opens the category the link names, else the first needing attention', () => {
  const { categories } = issuesSummary();
  expect(pickCategory(categories, 'no_cover')?.kind).toBe('no_cover');
  expect(pickCategory(categories, undefined)?.kind).toBe('scan_error');
  expect(pickCategory(categories, 'bogus')?.kind).toBe('scan_error');
  const clear = categories.map((c) => ({ ...c, count: 0 }));
  expect(pickCategory(clear, undefined)?.kind).toBe('scan_error');
  expect(pickCategory([], undefined)).toBeUndefined();
});

it('counts what needs attention, offline libraries included', () => {
  const s = issuesSummary({
    offline: [{ library_id: 2, name: 'Kids', root: '/mnt/kids', books: 3, listeners: 1 }],
  });
  expect(attentionTotal(s)).toBe(1 + 1 + 2 + 1);
});
