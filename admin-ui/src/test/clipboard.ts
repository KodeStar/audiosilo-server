import { vi } from 'vitest';

/**
 * jsdom has no execCommand and is not a secure context, so copyText takes the
 * textarea fallback: stand in for execCommand. Take it out again with
 * unstubExecCommand in an afterEach.
 */
export function stubExecCommand(impl: (command: string) => boolean) {
  const spy = vi.fn(impl);
  Object.defineProperty(document, 'execCommand', { value: spy, configurable: true });
  return spy;
}

export function unstubExecCommand() {
  Reflect.deleteProperty(document, 'execCommand');
}
