import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { copyText } from './clipboard';

// jsdom has no execCommand and is not a secure context, so copyText takes the
// textarea fallback; each test supplies the execCommand it needs.
function stubExecCommand(impl: (command: string) => boolean) {
  const spy = vi.fn(impl);
  Object.defineProperty(document, 'execCommand', { value: spy, configurable: true });
  return spy;
}

describe('copyText', () => {
  let button: HTMLButtonElement;

  beforeEach(() => {
    button = document.createElement('button');
    document.body.append(button);
    button.focus();
  });

  afterEach(() => {
    button.remove();
    Reflect.deleteProperty(document, 'execCommand');
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
