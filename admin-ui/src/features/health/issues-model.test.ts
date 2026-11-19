import en from '@/i18n/locales/en.json';
import { adminBook } from '@/test/library-fixtures';
import { bridgeJob, issuesSummary } from '@/test/fixtures';
import {
  attentionTotal,
  chaptersCheckNote,
  detectChosenNote,
  detectRowJob,
  fixFor,
  footKey,
  issueDetail,
  issueReason,
  newestJobByBook,
  pickCategory,
  rowFix,
} from './issues-model';

it("reads each book's newest detection, and what its row shows of it (BRIDGE-08)", () => {
  const newest = bridgeJob({ id: 9, state: 'running' });
  const byBook = newestJobByBook([
    newest,
    bridgeJob({ id: 8, state: 'failed' }),
    bridgeJob({ id: 7, path: 'Mosse/Past Tense', state: 'cancelled' }),
    bridgeJob({ id: 6, library_id: null, path: null }),
  ]);
  expect([...byBook.values()].map((j) => j.id)).toEqual([9, 7]);
  expect(newestJobByBook(undefined).size).toBe(0);
  // Open (queued, working, parked) holds the row's Detect back; how one ended shows beside it.
  expect(detectRowJob(newest)).toEqual({ job: newest, open: true });
  expect(detectRowJob(bridgeJob({ state: 'needs_attention' }))?.open).toBe(true);
  expect(detectRowJob(bridgeJob({ state: 'failed' }))?.open).toBe(false);
  expect(detectRowJob(bridgeJob({ state: 'done' }))?.open).toBe(false);
  expect(detectRowJob(bridgeJob({ state: 'cancelled' }))).toBeUndefined();
  expect(detectRowJob(undefined)).toBeUndefined();
});

it('offers Detect chapters only for a book with no chosen source and no edited chapters', () => {
  expect(rowFix('detect', adminBook())).toBe('detect');
  expect(detectChosenNote('detect', adminBook())).toBeUndefined();
  const chosen = adminBook({ chapter_choice: 'files' });
  expect(rowFix('detect', chosen)).toBeNull();
  expect(detectChosenNote('detect', chosen)).toBe('health.detect.chosen.files');
  // Edited chapters stand over found ones, choice or not.
  const edited = adminBook({ chapters_source: 'edited' });
  expect(rowFix('detect', edited)).toBeNull();
  expect(detectChosenNote('detect', edited)).toBe('health.detect.edited');
  // Another category's fix is untouched.
  expect(rowFix('join', edited)).toBe('join');
});

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

it('never says a book not matched has no ASIN: one from its tags is only a hint (server#307)', () => {
  expect(issueReason('unmatched', adminBook({ asin: 'B0TAGHINT1' }))).toEqual({
    key: 'health.reason.unmatched',
  });
  expect((en as Record<string, string>)['health.reason.unmatched']).not.toMatch(/ASIN|ISBN/);
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

it('offers Detect chapters for books without chapters only while the Bridge can', () => {
  expect(fixFor('no_chapters', true)).toBe('detect');
  expect(fixFor('no_chapters', false)).toBeNull();
  expect(fixFor('detailed_chapters', false)).toBe('chapters');
  expect(fixFor('scan_error', true)).toBe('rescan');
});

it("says Detect chapters writes to the book's .audiosilo folder, and only on its list", () => {
  expect(footKey(false, 'detect')).toBe('health.footDetect');
  expect(footKey(false, null)).toBe('health.foot');
  expect(footKey(false, 'chapters')).toBe('health.foot');
  expect(footKey(true, 'detect')).toBe('health.footIgnored');
});

it('reads a file that is not audio as one plain line, its prober message behind Details', () => {
  const probe = adminBook({
    scan_error: 'probe_failed',
    scan_error_file: 'Author/Book/07.mp3',
    scan_error_detail:
      '[mp3 @ 0xcbcc3c000] Failed to find two consecutive MPEG audio frames.\n[mov,mp4,m4a @ 0x7DE824000] moov atom not found',
  });
  expect(issueReason('scan_error', probe)).toEqual({
    key: 'health.reason.probe_failed',
    values: { file: '07.mp3', detail: '' },
  });
  // An ffprobe time-out is no sign of a damaged file: its own line (server#360).
  expect(
    issueReason('scan_error', { ...probe, scan_error_detail: 'ffprobe timed out after 30s' }),
  ).toEqual({ key: 'health.reason.probe_timeout', values: { file: '07.mp3', detail: '' } });
  // FFmpeg's context addresses go, for a row stored before the server dropped them.
  expect(issueDetail('scan_error', probe)).toBe(
    '[mp3] Failed to find two consecutive MPEG audio frames.\n[mov,mp4,m4a] moov atom not found',
  );
  // No toggle without a message, for another code, or another category.
  expect(issueDetail('scan_error', { ...probe, scan_error_detail: ' ' })).toBeUndefined();
  expect(
    issueDetail('scan_error', { scan_error: 'unreadable', scan_error_detail: 'permission denied' }),
  ).toBeUndefined();
  expect(issueDetail('no_cover', probe)).toBeUndefined();
  // An unopenable file still says why in its line.
  expect(
    issueReason(
      'scan_error',
      adminBook({ scan_error: 'unreadable', scan_error_detail: 'permission denied' }),
    ).values,
  ).toMatchObject({ detail: 'permission denied' });
});
