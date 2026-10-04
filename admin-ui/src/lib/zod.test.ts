import { z } from './zod';

// Under the console's CSP zod's eval probe would be reported as a script-src
// violation (a real browser shows it; jsdom doesn't enforce CSP), so the
// console's zod must run jitless.
it('runs zod jitless', () => {
  expect(z.config().jitless).toBe(true);
  expect(z.object({ name: z.string() }).parse({ name: 'x' })).toEqual({ name: 'x' });
});
