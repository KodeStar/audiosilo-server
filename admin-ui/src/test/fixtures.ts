import type {
  AdminLibrary,
  AdminSettings,
  AdminShare,
  AdminStats,
  Invite,
  InviteCreated,
  ServerInfo,
  User,
  UserDetail,
} from '@/api/types';

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

/** No scan running. */
export const idle = { running: false, total: 0, done: 0, indexed: 0 };

export function libraries(over: Partial<AdminLibrary>[] = []): AdminLibrary[] {
  const base: AdminLibrary[] = [
    {
      id: 1,
      name: 'Fiction',
      root: '/mnt/tank/fiction',
      default_view: '',
      sort_order: 0,
      book_count: 2400,
      available: true,
      scan: idle,
    },
    {
      id: 2,
      name: 'Kids',
      root: '/mnt/nas/kids',
      default_view: '',
      sort_order: 1,
      book_count: 849,
      available: true,
      scan: idle,
    },
  ];
  return base.map((l, i) => ({ ...l, ...over[i] }));
}

export const sam: User = {
  id: 2,
  username: 'sam',
  role: 'user',
  disabled: false,
  has_password: false,
  has_recovery: false,
  is_demo: false,
  last_seen_at: new Date(Date.now() - 3600_000).toISOString(),
};

export const users: User[] = [admin, sam];

export const kidsShare: AdminShare = {
  id: 7,
  name: 'Cosy mysteries',
  description: '',
  read_only: false,
  paths: [{ library_id: 1, path: 'Agatha Christie' }],
  member_ids: [2],
};

export const fictionGrant: AdminShare = {
  id: 8,
  name: 'Library: Fiction',
  description: 'Whole library',
  read_only: false,
  paths: [{ library_id: 1, path: '' }],
  whole_library_id: 1,
  member_ids: [],
};

export function invite(over: Partial<Invite> = {}): Invite {
  return {
    id: 31,
    label: 'invite',
    max_uses: 5,
    uses: 1,
    expires_at: new Date(Date.now() + 2 * 86400_000).toISOString(),
    redeemed_at: new Date(Date.now() - 3600_000).toISOString(),
    created_at: new Date(Date.now() - 5 * 86400_000).toISOString(),
    user_id: 2,
    username: 'sam',
    ...over,
  };
}

export function samDetail(over: Partial<UserDetail> = {}): UserDetail {
  return {
    user: sam,
    accessible_libraries: [],
    shares: [kidsShare],
    auth_codes: [invite()],
    ...over,
  };
}

export const created: InviteCreated = {
  auth_code: 'ABCD-1234',
  invite_url: 'https://books.example/connect#code=ABCD-1234',
  max_uses: 5,
  expires_at: new Date(Date.now() + 7 * 86400_000 + 60_000).toISOString(),
};
