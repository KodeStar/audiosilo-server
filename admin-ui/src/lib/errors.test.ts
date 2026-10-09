import { ApiError } from '@/api/client';
import i18n from '@/i18n';
import { errorMessage } from './errors';

describe('errorMessage', () => {
  const t = i18n.getFixedT('en');

  it('turns coded server errors into guidance, whatever the English message says', () => {
    expect(errorMessage(new ApiError(409, 'anything', 'username_taken'), t)).toMatch(/already/);
    expect(errorMessage(new ApiError(409, 'reworded', 'last_admin'), t)).toMatch(
      /at least one admin/,
    );
    expect(errorMessage(new ApiError(413, 'the image is larger than 5 MB', 'too_large'), t)).toBe(
      'That image is larger than 5 MB. Pick a smaller one.',
    );
    expect(errorMessage(new ApiError(404, 'no book', 'book_not_found'), t)).toMatch(
      /isn't in the library/,
    );
    expect(errorMessage(new ApiError(404, 'turned off', 'metadata_off'), t)).toBe(
      'Community metadata is off. Turn it on in Server settings first.',
    );
    expect(errorMessage(new ApiError(409, 'no copy', 'not_mirror_mode'), t)).toMatch(
      /^This server isn't keeping a local copy/,
    );
  });

  it('words every import code the server sends', () => {
    const codes = [
      'invalid_url',
      'invalid_import',
      'import_running',
      'import_not_found',
      'import_not_ready',
      'import_not_applied',
      'import_applied',
      'abs_unreachable',
      'abs_unauthorized',
      'not_abs',
      'fetch_failed',
    ];
    for (const code of codes) {
      expect(errorMessage(new ApiError(400, 'server words', code), t)).not.toBe('server words');
    }
    expect(errorMessage(new ApiError(400, 'cutoff must be "auto"', 'invalid_import'), t)).toMatch(
      /^This server couldn't start the import/,
    );
  });

  it('passes other server messages through and explains network failures', () => {
    expect(errorMessage(new ApiError(500, 'could not list shares'), t)).toBe(
      'could not list shares',
    );
    expect(errorMessage(new ApiError(409, 'odd', 'unknown_code'), t)).toBe('odd');
    expect(errorMessage(new ApiError(429, 'too many requests'), t)).toMatch(/Too many/);
    expect(errorMessage(new TypeError('Failed to fetch'), t)).toMatch(/didn't answer/);
  });
  it('words a refused field by its reason, else as the server said it', () => {
    const t = i18n.getFixedT('en');
    const refused = (reason: string | undefined, max?: number) =>
      new ApiError(400, 'the server says no', 'invalid_target', 'name', { reason, max });
    expect(errorMessage(refused('name_too_long', 64), t)).toBe(
      'A name can be at most 64 characters.',
    );
    expect(errorMessage(refused('url_ntfy'), t)).toMatch(/^Enter the topic's address/);
    expect(errorMessage(refused('something_new'), t)).toBe('the server says no');
    expect(errorMessage(refused(undefined), t)).toBe('the server says no');
  });
});
