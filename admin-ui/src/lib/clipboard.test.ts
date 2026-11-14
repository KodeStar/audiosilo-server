import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { stubExecCommand, stubShare, unstubExecCommand } from '@/test/clipboard';
import { copyText, copyWithToast, shareOrCopy } from './clipboard';
import { toast } from './toast';

describe('copyWithToast', () => {
  afterEach(() => {
    unstubExecCommand();
    vi.restoreAllMocks();
  });

  it('keeps the refused-copy warning open until closed, showing the text', async () => {
    stubExecCommand(() => false);
    const add = vi.spyOn(toast, 'add');
    await copyWithToast('/books/Dune', 'Path');
    expect(add).toHaveBeenCalledWith(
      expect.objectContaining({ description: '/books/Dune', type: 'warning', timeout: 0 }),
    );
  });
});

describe('copyText', () => {
  let button: HTMLButtonElement;

  beforeEach(() => {
    button = document.createElement('button');
    document.body.append(button);
    button.focus();
  });

  afterEach(() => {
    button.remove();
    unstubExecCommand();
  });

  it('copies through a hidden textarea and takes it out again', async () => {
    let copied = '';
    const exec = stubExecCommand(() => {
      const ta = document.activeElement as HTMLTextAreaElement;
      copied = ta.value.slice(ta.selectionStart, ta.selectionEnd);
      return true;
    });
    expect(await copyText('/books/Dune')).toBe(true);
    expect(exec).toHaveBeenCalledWith('copy');
    expect(copied).toBe('/books/Dune');
    expect(document.querySelector('textarea')).toBeNull();
    expect(document.activeElement).toBe(button);
  });

  it('places the textarea beside the focused element, inside its popup', async () => {
    const popup = document.createElement('div');
    popup.setAttribute('role', 'dialog');
    document.body.append(popup);
    popup.append(button);
    button.focus();
    let parent: Element | null = null;
    stubExecCommand(() => {
      parent = document.activeElement?.parentElement ?? null;
      return true;
    });
    expect(await copyText('x')).toBe(true);
    expect(parent).toBe(popup);
    expect(document.activeElement).toBe(button);
    popup.remove();
  });

  it.each([
    ['refuses', () => false],
    [
      'throws',
      () => {
        throw new Error('denied');
      },
    ],
  ])('removes the textarea and reports failure when execCommand %s', async (_, impl) => {
    stubExecCommand(impl);
    expect(await copyText('x')).toBe(false);
    expect(document.querySelector('textarea')).toBeNull();
    expect(document.activeElement).toBe(button);
  });
});

describe('shareOrCopy', () => {
  const data = {
    title: 'Invite',
    text: 'Come and listen.',
    url: 'https://books.example.com/connect',
  };
  afterEach(() => {
    stubShare(undefined);
    unstubExecCommand();
    vi.restoreAllMocks();
  });

  it('hands it to the share sheet, and closing that is no failure', async () => {
    const exec = stubExecCommand(() => true);
    const share = vi.fn().mockRejectedValue(new DOMException('closed', 'AbortError'));
    stubShare(share);
    await shareOrCopy(data, 'Invite message');
    expect(share).toHaveBeenCalledWith(data);
    expect(exec).not.toHaveBeenCalled();
  });

  it('ignores a press while the share sheet is still open (server#386)', async () => {
    const exec = stubExecCommand(() => true);
    let close = () => {};
    const share = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          close = resolve;
        }),
    );
    stubShare(share);
    const first = shareOrCopy(data, 'Invite message');
    await shareOrCopy(data, 'Invite message');
    expect(share).toHaveBeenCalledTimes(1);
    expect(exec).not.toHaveBeenCalled();
    close();
    await first;
    // Closed, the next press opens it again.
    const again = shareOrCopy(data, 'Invite message');
    expect(share).toHaveBeenCalledTimes(2);
    close();
    await again;
  });

  it('copies the text and the link when there is no share sheet, or it refuses', async () => {
    let copied = '';
    stubExecCommand(() => {
      copied = (document.activeElement as HTMLTextAreaElement).value;
      return true;
    });
    await shareOrCopy(data, 'Invite message');
    expect(copied).toBe('Come and listen.\nhttps://books.example.com/connect');
    copied = '';
    stubShare(vi.fn().mockRejectedValue(new DOMException('no', 'NotAllowedError')));
    await shareOrCopy(data, 'Invite message');
    expect(copied).toBe('Come and listen.\nhttps://books.example.com/connect');
  });
});
