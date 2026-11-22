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
    expect(errorMessage(new ApiError(400, 'too many rules', 'too_large'), t)).toBe(
      "That's too large for the server to take in one go. Try with less at a time.",
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
    expect(errorMessage(new ApiError(409, 'turn it on', 'report_off'), t)).toMatch(
      /^Help complete the community database is off/,
    );
  });

  it("words a code for one action over the general wording (a cover's too_large)", () => {
    const tooBig = new ApiError(413, 'the image is larger than 5 MB', 'too_large');
    expect(errorMessage(tooBig, t, { too_large: 'book.cover.tooLarge' })).toBe(
      'That image is larger than 5 MB. Pick a smaller one.',
    );
    expect(errorMessage(tooBig, t)).not.toMatch(/image/);
    // A code the action doesn't word keeps the general wording.
    expect(
      errorMessage(new ApiError(409, 'x', 'last_admin'), t, { too_large: 'book.cover.tooLarge' }),
    ).toMatch(/at least one admin/);
  });

  it('words an Audible destination or template the server refuses (bridge#47)', () => {
    expect(
      errorMessage(
        new ApiError(400, 'invalid destination: no hidden folders', 'invalid_dest_path'),
        t,
      ),
    ).toMatch(/^That isn't a place inside the library/);
    expect(
      errorMessage(new ApiError(400, 'invalid template: unknown {Foo}', 'invalid_template'), t),
    ).toMatch(/^That template doesn't work/);
  });

  it('words every household member refusal the server sends', () => {
    const codes = [
      'member_not_admin',
      'owner_is_member',
      'owner_is_demo',
      'self_managed',
      'has_members',
      'member_fields_required',
      'invalid_keep_members',
      'kid_account',
    ];
    for (const code of codes) {
      const text = errorMessage(new ApiError(409, 'server words', code), t);
      expect(text, code).not.toBe('server words');
      expect(text, code).not.toMatch(/^errors\./);
    }
    // The admin minting or rotating an invite for a kid.
    expect(errorMessage(new ApiError(409, 'x', 'kid_account'), t)).toBe(
      "Kids don't use invites. The person who manages the account sets up their devices in the player.",
    );
    expect(errorMessage(new ApiError(409, 'x', 'member_not_admin'), t)).toBe(
      "A household member can't be an admin. Make them independent first.",
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
      'abs_certificate',
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
  it("words a backup's stored template or destination by whose setting it is", () => {
    const t = i18n.getFixedT('en');
    const stored = (code: string, reason?: string) =>
      new ApiError(
        400,
        'invalid: template: invalid template: unknown variable {Bogus}',
        code,
        undefined,
        {
          reason,
        },
      );
    expect(errorMessage(stored('invalid_template', 'account'), t)).toBe(
      "This account's placement template no longer works. Fix it in Account settings and back up again.",
    );
    expect(errorMessage(stored('invalid_dest_path', 'title'), t)).toBe(
      "This title's destination is no longer valid. Set it again with Destination and back up again.",
    );
    expect(errorMessage(stored('invalid_dest_path', 'indexed_book'), t)).toMatch(
      /inside an indexed book/,
    );
    expect(errorMessage(stored('invalid_dest_path', 'book_override'), t)).toMatch(
      /folder setting would join/,
    );
    // The value just typed (the account and title PATCHes): no reason.
    expect(errorMessage(stored('invalid_template'), t)).toMatch(/^That template doesn't work/);
  });
});
