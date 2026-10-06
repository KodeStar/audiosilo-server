import { QueryClient } from '@tanstack/react-query';
import { mockFetch } from '@/test/fetch-mock';
import { keys, setFolderMode } from './hooks';

// Setting how a folder reads, from any screen (Folders, the detection dialog,
// Health's join), refreshes every folder listing of that library, so the change
// shows on the others too.

afterEach(() => vi.unstubAllGlobals());

it('saves the folder mode and refreshes the library folder listings', async () => {
  const calls = mockFetch({
    'PUT /admin/libraries/1/folder-override': { body: { status: 'override set' } },
  });
  const qc = new QueryClient();
  const listings = [keys.browse(1, ''), keys.browse(1, 'Cowell'), keys.browse(2, '')];
  for (const key of listings) qc.setQueryData(key, { entries: [] });

  await setFolderMode(qc, 1, 'Cowell/Dragonese', 'book');

  const put = calls.find((c) => c.method === 'PUT');
  expect(put?.query.get('path')).toBe('Cowell/Dragonese');
  expect(put?.body).toEqual({ mode: 'book' });
  expect(qc.getQueryState(listings[0])?.isInvalidated).toBe(true);
  expect(qc.getQueryState(listings[1])?.isInvalidated).toBe(true);
  // Another library's listings are left alone.
  expect(qc.getQueryState(listings[2])?.isInvalidated).toBe(false);
});
