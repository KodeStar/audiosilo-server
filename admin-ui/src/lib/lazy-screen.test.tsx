import { Component, type ReactNode } from 'react';
import { render, screen } from '@testing-library/react';
import { lazyScreen } from './lazy-screen';

function Hello({ name }: { name: string }) {
  return <h1>Hello {name}</h1>;
}

/** A chunk whose import the test settles. */
function chunk() {
  let settle!: { resolve: (m: { Hello: typeof Hello }) => void; reject: (e: Error) => void };
  const load = vi.fn(
    () =>
      new Promise<{ Hello: typeof Hello }>((resolve, reject) => {
        settle = { resolve, reject };
      }),
  );
  return { load, settle: () => settle };
}

class Boundary extends Component<{ children: ReactNode }, { error?: Error }> {
  state: { error?: Error } = {};
  static getDerivedStateFromError(error: Error) {
    return { error };
  }
  render() {
    return this.state.error ? <p>failed: {this.state.error.message}</p> : this.props.children;
  }
}

describe('lazyScreen', () => {
  it('renders a preloaded screen on its first render, with no skeleton', async () => {
    const c = chunk();
    const Screen = lazyScreen(c.load, (m) => m.Hello);
    const preloaded = Screen.preload();
    c.settle().resolve({ Hello });
    await preloaded;
    const { container } = render(<Screen name="Ada" />);
    // Synchronously: no skeleton, so nothing waits on a Suspense fallback.
    expect(container.querySelector('[aria-busy]')).toBeNull();
    expect(screen.getByRole('heading', { name: 'Hello Ada' })).toBeInTheDocument();
    expect(c.load).toHaveBeenCalledTimes(1);
  });

  it('shows the skeleton until the chunk is in, fetching it once', async () => {
    const c = chunk();
    const Screen = lazyScreen(c.load, (m) => m.Hello);
    void Screen.preload(); // a hover, then the visit
    render(<Screen name="Ada" />);
    expect(screen.getByRole('status')).toHaveAttribute('aria-busy', 'true');
    c.settle().resolve({ Hello });
    expect(await screen.findByRole('heading', { name: 'Hello Ada' })).toBeInTheDocument();
    expect(screen.queryByRole('status')).toBeNull();
    expect(c.load).toHaveBeenCalledTimes(1);
  });

  it('throws a chunk that fails to the error boundary, and fetches again next time', async () => {
    const c = chunk();
    const Screen = lazyScreen(c.load, (m) => m.Hello);
    vi.spyOn(console, 'error').mockImplementation(() => {});
    render(
      <Boundary>
        <Screen name="Ada" />
      </Boundary>,
    );
    c.settle().reject(new Error('offline'));
    expect(await screen.findByText('failed: offline')).toBeInTheDocument();
    // A failed fetch isn't kept: the next visit asks again.
    const again = Screen.preload();
    expect(c.load).toHaveBeenCalledTimes(2);
    c.settle().resolve({ Hello });
    await again;
  });
});
