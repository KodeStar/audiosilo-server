import { COLLECT_MS, createBatcher } from './batcher';

// The shared machinery behind cover-batch and work-batch: items asked for in one
// window go out together, chunked, each caller answered from its own item.

/** A batcher answering each item doubled, recording what each send carried. */
function doubler(limit = 3) {
  const sent: number[][] = [];
  const send = vi.fn(async (items: number[]) => {
    sent.push(items);
    return items.map((n) => n * 2);
  });
  return { load: createBatcher({ key: String, limit, send }), send, sent };
}

afterEach(() => vi.useRealTimers());

describe('createBatcher', () => {
  it('coalesces the items asked for in one window, each key once', async () => {
    const { load, sent } = doubler();
    expect(await Promise.all([load(1), load(2), load(1)])).toEqual([2, 4, 2]);
    expect(sent).toEqual([[1, 2]]);

    // A later window is a new batch.
    expect(await load(3)).toBe(6);
    expect(sent).toEqual([[1, 2], [3]]);
  });

  it('waits one window before sending', async () => {
    vi.useFakeTimers();
    const { load, send } = doubler();
    const answer = load(1);
    vi.advanceTimersByTime(COLLECT_MS - 1);
    expect(send).not.toHaveBeenCalled();
    const later = load(2); // still in the window
    vi.advanceTimersByTime(1);
    expect(send).toHaveBeenCalledTimes(1);
    expect(await Promise.all([answer, later])).toEqual([2, 4]);
  });

  it('chunks a window into sends of at most limit', async () => {
    const { load, sent } = doubler(2);
    expect(await Promise.all([1, 2, 3, 4, 5].map((n) => load(n)))).toEqual([2, 4, 6, 8, 10]);
    expect(sent).toEqual([[1, 2], [3, 4], [5]]);
  });

  it('rejects an aborted caller, still answering another waiting on the same item', async () => {
    const { load, sent } = doubler();
    const gone = new AbortController();
    const dropped = load(1, gone.signal);
    const kept = load(1);
    gone.abort(new Error('gone'));
    await expect(dropped).rejects.toThrow('gone');
    expect(await kept).toBe(2);
    expect(sent).toEqual([[1]]);
  });

  it('drops an item every caller gave up on before the batch went out', async () => {
    const { load, sent } = doubler();
    const a = new AbortController();
    const b = new AbortController();
    const dropped = [load(1, a.signal), load(1, b.signal)];
    const kept = load(2);
    a.abort();
    b.abort();
    await Promise.all(dropped.map((p) => expect(p).rejects.toBeDefined()));
    expect(await kept).toBe(4);
    expect(sent).toEqual([[2]]);
  });

  it('rejects at once for a signal already aborted, and sends nothing', async () => {
    const { load, send } = doubler();
    const gone = new AbortController();
    gone.abort(new Error('already'));
    await expect(load(1, gone.signal)).rejects.toThrow('already');
    await new Promise((r) => setTimeout(r, COLLECT_MS * 2));
    expect(send).not.toHaveBeenCalled();
  });

  it('rejects every caller of a chunk whose send failed, and only that chunk', async () => {
    const send = vi.fn(async (items: number[]) => {
      if (items.includes(1)) throw new Error('down');
      return items;
    });
    const load = createBatcher({ key: String, limit: 2, send });
    const results = await Promise.allSettled([load(1), load(2), load(3)]);
    expect(results.map((r) => r.status)).toEqual(['rejected', 'rejected', 'fulfilled']);
  });

  it('stops listening for an abort once the caller is answered or failed', async () => {
    const answered = new AbortController();
    const failed = new AbortController();
    const off = [
      vi.spyOn(answered.signal, 'removeEventListener'),
      vi.spyOn(failed.signal, 'removeEventListener'),
    ];
    const ok = doubler().load(1, answered.signal);
    const bad = createBatcher({
      key: String,
      limit: 1,
      send: () => Promise.reject(new Error('down')),
    })(1, failed.signal);
    await ok;
    await expect(bad).rejects.toThrow('down');
    for (const spy of off) expect(spy).toHaveBeenCalledWith('abort', expect.any(Function));
  });
});
