import type { AdminSettings, AdminStats, ServerInfo, User } from '@/api/types';

export const serverInfo: ServerInfo = {
  name: 'AudioSilo',
  server_id: 'srv-1',
  version: '0.9.2',
  api: 'v1',
  capabilities: {
    admin_ui: true,
    web_player: true,
    transcode: true,
    upload: false,
    websocket: false,
    api_keys: true,
    export: true,
    metadata: true,
  },
  auth: { methods: ['auth_code', 'password'] },
  demo: { enabled: false },
};

export const admin: User = {
  id: 1,
  username: 'chris',
  role: 'admin',
  disabled: false,
  has_password: true,
  has_recovery: false,
  is_demo: false,
};

export const member: User = { ...admin, id: 2, username: 'sam', role: 'user' };

export function stats(over: Partial<AdminStats> = {}): AdminStats {
  const now = Date.now();
  return {
    total_books: 3249,
    total_libraries: 2,
    total_users: 7,
    libraries: [
      { id: 1, name: 'Fiction', book_count: 2400 },
      { id: 2, name: 'Kids', book_count: 849 },
    ],
    listening: [
      {
        user_id: 2,
        username: 'sam',
        library_id: 1,
        path: 'Andy Weir/Project Hail Mary',
        title: 'Project Hail Mary',
        author: 'Andy Weir',
        position: 3000,
        duration: 6000,
        finished: false,
        updated_at: new Date(now - 60_000).toISOString(),
      },
      {
        user_id: 3,
        username: 'maya',
        library_id: 2,
        path: 'Peter Brown/The Wild Robot',
        title: 'The Wild Robot',
        author: 'Peter Brown',
        position: 100,
        duration: 100,
        finished: true,
        updated_at: new Date(now - 3 * 3600_000).toISOString(),
      },
    ],
    ...over,
  };
}

export const settings: AdminSettings = {
  metadata: { enabled: true, base_url: 'https://meta.audiosilo.app', available: true },
};
