import { z } from 'zod';

// zod compiles object schemas with `new Function` when eval works, and probes for
// it once. Under the console's CSP (no 'unsafe-eval') the probe throws, which zod
// catches, but the browser still reports a script-src violation. jitless skips
// the probe and the compiler. Import z from here, never from 'zod' (ESLint).
z.config({ jitless: true });

export { z };
