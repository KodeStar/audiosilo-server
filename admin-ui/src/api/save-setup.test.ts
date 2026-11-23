import { QueryClient } from '@tanstack/react-query';
import { mockFetch, type MockReply } from '@/test/fetch-mock';
import { api } from './client';
import { keys, saveSetup } from './hooks';
import type { SetupEnvelope, SetupState } from './types';

// A setup read may complete after a write. Keep the saved state.

afterEach(() => vi.unstubAllGlobals());

const envelope = (state: SetupState): SetupEnvelope => ({
  state,
  request: {} as SetupEnvelope['request'],
  config_libraries: [],
  mount_hints: [],
});

it('keeps the saved setup state over a read that answers after the save', async () => {
  const before: SetupState = { finished: true, dismissed: false, steps: {} };
  const after: SetupState = { ...before, dismissed: true };
  let answerRead: (reply: MockReply) => void = () => {};
  const slowRead = new Promise<MockReply>((resolve) => (answerRead = resolve));
  mockFetch({
    'GET /admin/setup': () => slowRead,
    'PUT /admin/setup': { body: after },
  });
  const qc = new QueryClient();
  qc.setQueryData(keys.setup, envelope(before));

  // The earlier read is still out when the card is dismissed.
  const read = qc.fetchQuery({ queryKey: keys.setup, queryFn: api.setup }).catch(() => undefined);
  await saveSetup(qc, { dismissed: true });
  answerRead({ body: envelope(before) });
  await read;

  expect(qc.getQueryData<SetupEnvelope>(keys.setup)?.state).toEqual(after);
});
