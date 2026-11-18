import { describe, expect, it } from 'vitest';
import en from '@/i18n/locales/en.json';
import { PODCAST_FETCH_CODES, podcastJobReason } from './podcast-reason';

const failed = (kind: string, result: Record<string, unknown> = {}) =>
  podcastJobReason({ kind, state: 'failed', result });

describe('podcastJobReason', () => {
  it("words a fetch's codes as bridge.fetchMessages does", () => {
    expect(failed('podcast.fetch', { code: 'gone' })).toEqual({ key: 'bridge.podcastCode.gone' });
    expect(failed('podcast.fetch', { code: 'http_404' })).toEqual({
      key: 'bridge.podcastCode.http',
      values: { status: '404' },
    });
    // An older Bridge's redirect loop, not a status.
    expect(failed('podcast.fetch', { code: 'http_310' })).toEqual({
      key: 'bridge.podcastCode.redirect_loop',
    });
    // A failed job without a code is a lost lease (bridge.PodcastOutcomeOf).
    expect(failed('podcast.fetch')).toEqual({ key: 'bridge.podcastCode.bridge_lost' });
    expect(failed('podcast.fetch', { code: 'brand_new' })).toBeUndefined();
    expect(failed('podcast.fetch', { code: 'http_999' })).toBeUndefined();
  });

  it("words a poll's codes as the failing-feed notice does", () => {
    expect(failed('podcast.poll', { code: 'gone' })).toEqual({
      key: 'events.podcast_feed_failing.code.gone',
    });
    expect(failed('podcast.poll', { code: 'http_503' })).toEqual({
      key: 'events.podcast_feed_failing.http',
      values: { status: '503' },
    });
    expect(failed('podcast.poll')).toEqual({ key: 'bridge.podcastCode.bridge_lost' });
    expect(failed('podcast.poll', { code: 'library_not_writable' })).toEqual({
      key: 'bridge.podcastCode.library_not_writable',
    });
    // A code neither the notice nor the poll's own list words: the Bridge's text.
    expect(failed('podcast.poll', { code: 'write_failed' })).toBeUndefined();
  });

  it('leaves other kinds and jobs that did not fail alone', () => {
    expect(failed('podcast.subscribe', { code: 'gone' })).toBeUndefined();
    expect(failed('bridge.verify', { code: 'gone' })).toBeUndefined();
    expect(
      podcastJobReason({ kind: 'podcast.fetch', state: 'done', result: { code: 'gone' } }),
    ).toBeUndefined();
  });

  it('has an English sentence for every code it words', () => {
    const keys = [...PODCAST_FETCH_CODES, 'http'].map((c) => `bridge.podcastCode.${c}`);
    const dict = en as Record<string, string>;
    expect(keys.filter((k) => !dict[k])).toEqual([]);
  });
});
