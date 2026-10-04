import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { BackupsEnvelope, NotifyTarget, NotifyTargetsEnvelope } from '@/api/types';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { settingsWith, systemStatus } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';
import { EVENT_KINDS } from '@/lib/server-events';

// Settings > Backups and Settings > Notifications.

function backupsEnv(over: Partial<BackupsEnvelope> = {}): BackupsEnvelope {
  return {
    backups: [
      {
        name: 'audiosilo-20261004-030000Z-scheduled.db',
        size: 4_200_000,
        created_at: '2026-10-04T03:00:00Z',
        kind: 'scheduled',
      },
    ],
    status: {
      dir: '/data/backups',
      running: false,
      last: null,
      latest: null,
      next: '2026-10-05T03:00:00Z',
    },
    restore: { pending: null, last: null },
    ...over,
  };
}

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/system': { body: systemStatus() },
    'GET /admin/backups': { body: backupsEnv() },
    ...over,
  });
}

beforeEach(() => setToken('stored'));

describe('backups', () => {
  it('lists the backups and saves a weekly schedule', async () => {
    const calls = mockFetch(
      routes({
        'PATCH /admin/settings': {
          body: settingsWith({ backups: { schedule: 'weekly:sun:03:00', keep: 7, dir: '' } }),
        },
      }),
    );
    renderApp('/server?topic=backups');
    expect(
      await screen.findByText('audiosilo-20261004-030000Z-scheduled.db', { exact: false }),
    ).toBeInTheDocument();
    expect(screen.getByText('/data/backups')).toBeInTheDocument();
    const user = userEvent.setup();
    await user.selectOptions(screen.getByRole('combobox', { name: 'Back up' }), 'weekly');
    expect(screen.getByRole('combobox', { name: 'Day' })).toHaveValue('sun');
    await user.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({
        backups: { schedule: 'weekly:sun:03:00' },
      }),
    );
  });

  it('backs up now and says when it is done', async () => {
    // Before the click nothing runs; the first poll after it sees the backup
    // running, the next one finished.
    let polls = -1;
    const status = backupsEnv().status;
    mockFetch(
      routes({
        'POST /admin/backups': () => {
          polls = 0;
          return { status: 202, body: backupsEnv({ status: { ...status, running: true } }) };
        },
        'GET /admin/backups': () => {
          if (polls < 0) return { body: backupsEnv() };
          const done = polls++ > 0;
          return {
            body: backupsEnv({
              status: {
                ...status,
                running: !done,
                last: done
                  ? {
                      at: '2026-10-04T15:00:00Z',
                      ok: true,
                      trigger: 'manual',
                      name: 'audiosilo-new.db',
                    }
                  : null,
              },
            }),
          };
        },
      }),
    );
    renderApp('/server?topic=backups');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Back up now' }));
    expect(screen.getByRole('button', { name: 'Backing up...' })).toBeDisabled();
    expect(await screen.findByText('Backup made', {}, { timeout: 4000 })).toBeInTheDocument();
  });

  it('asks for the word before scheduling a restore, then offers to cancel it', async () => {
    const pending = {
      name: 'audiosilo-20261004-030000Z-scheduled.db',
      requested_at: '2026-10-04T15:00:00Z',
      requested_by: 'admin',
      schema: '0019_audit_notifications.sql',
    };
    const calls = mockFetch(
      routes({
        'POST /admin/backups/audiosilo-20261004-030000Z-scheduled.db/restore': {
          body: backupsEnv({ restore: { pending, last: null } }),
        },
        'DELETE /admin/restore': { status: 204 },
      }),
    );
    renderApp('/server?topic=backups');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Restore...' }));
    const dialog = await screen.findByRole('dialog', { name: 'Restore this backup?' });
    const confirm = within(dialog).getByRole('button', { name: 'Restore at the next start' });
    expect(confirm).toBeDisabled();
    await user.type(within(dialog).getByRole('textbox'), 'restore');
    await user.click(confirm);
    expect(await screen.findByText('A restore is waiting for a restart')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Cancel restore' }));
    await waitFor(() =>
      expect(calls.some((c) => c.method === 'DELETE' && c.path === '/admin/restore')).toBe(true),
    );
  });

  it('says why the last backup failed and when there are none', async () => {
    mockFetch(
      routes({
        'GET /admin/backups': {
          body: backupsEnv({
            backups: [],
            status: {
              dir: '/data/backups',
              running: false,
              last: {
                at: '2026-10-04T03:00:00Z',
                ok: false,
                trigger: 'scheduled',
                error: 'disk_full',
              },
              latest: null,
              next: null,
            },
          }),
        },
      }),
    );
    renderApp('/server?topic=backups');
    expect(await screen.findByText(/The disk is full/)).toBeInTheDocument();
    expect(screen.getByText('No backups yet')).toBeInTheDocument();
  });
});

function target(over: Partial<NotifyTarget> = {}): NotifyTarget {
  return {
    id: 1,
    kind: 'ntfy',
    name: 'My phone',
    address: 'https://ntfy.sh/hear…',
    has_secret: false,
    enabled: true,
    events: ['scan_failed'],
    created_at: '2026-10-01T00:00:00Z',
    updated_at: '2026-10-01T00:00:00Z',
    last_at: null,
    last_ok: null,
    last_error: '',
    ...over,
  };
}

function targetsEnv(targets: NotifyTarget[]): NotifyTargetsEnvelope {
  return { targets, events: [...EVENT_KINDS], kinds: ['webhook', 'ntfy', 'discord'] };
}

describe('notifications', () => {
  it('adds a first destination with the problems switched on', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/notifications': { body: targetsEnv([]) },
        'POST /admin/notifications': { status: 201, body: target() },
      }),
    );
    renderApp('/server?topic=notifications');
    const user = userEvent.setup();
    expect(await screen.findByText('No destinations yet')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Add a destination' }));
    const dialog = await screen.findByRole('dialog', { name: 'Add a destination' });
    await user.click(within(dialog).getByRole('radio', { name: /ntfy/ }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Name' }), 'My phone');
    await user.type(
      within(dialog).getByRole('textbox', { name: 'Topic address' }),
      'https://ntfy.sh/hearthside',
    );
    await user.click(within(dialog).getByRole('button', { name: 'Add destination' }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === 'POST')?.body).toEqual({
        kind: 'ntfy',
        name: 'My phone',
        url: 'https://ntfy.sh/hearthside',
        events: ['scan_failed', 'library_unavailable', 'update_available', 'backup_failed'],
        enabled: true,
      }),
    );
  });

  it('shows a refusal on the field the server names', async () => {
    mockFetch(
      routes({
        'GET /admin/notifications': { body: targetsEnv([]) },
        'POST /admin/notifications': {
          status: 400,
          body: {
            error: 'must be a Discord webhook address',
            code: 'invalid_target',
            field: 'url',
          },
        },
      }),
    );
    renderApp('/server?topic=notifications');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Add a destination' }));
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('radio', { name: /Discord/ }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Name' }), 'Chat');
    const url = within(dialog).getByRole('textbox', { name: 'Discord webhook address' });
    await user.type(url, 'https://evil.example/x');
    await user.click(within(dialog).getByRole('button', { name: 'Add destination' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'must be a Discord webhook address',
    );
    expect(url).toHaveAttribute('aria-invalid', 'true');
  });

  it('says a secret must come again when the address moves to another server', async () => {
    mockFetch(
      routes({
        'GET /admin/notifications': {
          body: targetsEnv([
            target({
              kind: 'webhook',
              name: 'Hook',
              address: 'https://hooks.example.com/in…',
              has_secret: true,
            }),
          ]),
        },
      }),
    );
    renderApp('/server?topic=notifications');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Actions for Hook' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Edit' }));
    const dialog = await screen.findByRole('dialog', { name: 'Edit Hook' });
    expect(
      within(dialog).getByText('A secret is saved. Leave empty to keep it.'),
    ).toBeInTheDocument();
    const url = within(dialog).getByRole('textbox', { name: 'Webhook address' });
    await user.type(url, 'https://hooks.example.com/other');
    expect(
      within(dialog).getByText('A secret is saved. Leave empty to keep it.'),
    ).toBeInTheDocument();
    await user.clear(url);
    await user.type(url, 'https://elsewhere.example/in');
    expect(
      within(dialog).getByText(
        'This address is on another server: enter the secret again, or remove it.',
      ),
    ).toBeInTheDocument();
  });

  it('ticks an event for a destination and says why a test failed', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/notifications': {
          body: targetsEnv([
            target({ last_at: '2026-10-04T09:00:00Z', last_ok: false, last_error: 'http_404' }),
          ]),
        },
        'PATCH /admin/notifications/1': { body: target({ events: ['book_added', 'scan_failed'] }) },
        'POST /admin/notifications/1/test': {
          body: { ok: false, error: 'unreachable', target: target() },
        },
      }),
    );
    renderApp('/server?topic=notifications');
    const user = userEvent.setup();
    expect(await screen.findByText(/it answered HTTP 404/)).toBeInTheDocument();
    await user.click(screen.getByRole('checkbox', { name: 'Send "New books added" to My phone' }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({
        events: ['book_added', 'scan_failed'],
      }),
    );
    await user.click(screen.getByRole('button', { name: 'Send test' }));
    expect(await screen.findByText("The test didn't reach My phone")).toBeInTheDocument();
    expect(screen.getByText("the server couldn't connect")).toBeInTheDocument();
  });
});
