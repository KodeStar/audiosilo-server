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
  });

  it('passes other server messages through and explains network failures', () => {
    expect(errorMessage(new ApiError(500, 'could not list shares'), t)).toBe(
      'could not list shares',
    );
    expect(errorMessage(new ApiError(409, 'odd', 'unknown_code'), t)).toBe('odd');
    expect(errorMessage(new ApiError(429, 'too many requests'), t)).toMatch(/Too many/);
    expect(errorMessage(new TypeError('Failed to fetch'), t)).toMatch(/didn't answer/);
  });
});
