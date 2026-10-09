import { mirrorStatus } from '@/test/fixtures';
import { MIRROR_CLOCK_SKEW_MS, mirrorBusy } from './hooks';

describe('mirrorBusy', () => {
  const now = Date.parse('2026-10-09T12:00:00Z');
  const at = (ms: number) => new Date(now + ms).toISOString();

  it('follows a download and a running check', () => {
    expect(mirrorBusy(undefined, now)).toBe(false);
    expect(
      mirrorBusy(mirrorStatus({ state: 'downloading', next_check_at: at(86_400_000) }), now),
    ).toBe(true);
    expect(mirrorBusy(mirrorStatus({ next_check_at: undefined }), now)).toBe(true);
  });

  it("sees Check now's due-now even when the browser's clock lags the server's", () => {
    // The server answered with its own now, a few seconds ahead of this browser.
    expect(mirrorBusy(mirrorStatus({ next_check_at: at(5_000) }), now)).toBe(true);
    expect(mirrorBusy(mirrorStatus({ next_check_at: at(MIRROR_CLOCK_SKEW_MS + 1) }), now)).toBe(
      false,
    );
    expect(mirrorBusy(mirrorStatus({ next_check_at: at(86_400_000) }), now)).toBe(false);
  });
});
