import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { stubExecCommand, unstubExecCommand } from '@/test/clipboard';
import { copyText, copyWithToast } from './clipboard';
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
