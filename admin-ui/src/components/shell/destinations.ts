import { Activity, HeartPulse, LibraryBig, Server, Users, type LucideIcon } from 'lucide-react';

// The five top-bar destinations and their sub-bar sections (STYLEGUIDE.md
// section 2). Home is the mark, not a destination. Labels are i18n keys:
// `shell.dest.<key>` and `shell.section.<key>.<section>`.

export type DestinationKey = 'library' | 'people' | 'activity' | 'health' | 'server';

export interface Destination {
  key: DestinationKey;
  /** Router path; the optional section segment is omitted for the first section. */
  route: `/${DestinationKey}/{-$section}`;
  icon: LucideIcon;
  sections: readonly string[];
  /** The redesign phase that builds this destination's screens (shown while it's pending). */
  phase: string;
}

export const DESTINATIONS: readonly Destination[] = [
  {
    key: 'library',
    route: '/library/{-$section}',
    icon: LibraryBig,
    sections: ['books', 'authors', 'series', 'narrators', 'folders', 'libraries'],
    phase: '2b',
  },
  {
    key: 'people',
    route: '/people/{-$section}',
    icon: Users,
    sections: ['people', 'invites', 'shares', 'devices'],
    phase: '1b',
  },
  {
    key: 'activity',
    route: '/activity/{-$section}',
    icon: Activity,
    sections: ['overview', 'live', 'sessions', 'year'],
    phase: '4c',
  },
  {
    key: 'health',
    route: '/health/{-$section}',
    icon: HeartPulse,
    sections: ['issues', 'jobs', 'system'],
    phase: '3',
  },
  {
    key: 'server',
    route: '/server/{-$section}',
    icon: Server,
    sections: ['settings', 'logs', 'audit', 'about'],
    phase: '5a',
  },
];

export function destinationFor(pathname: string): Destination | undefined {
  // pathname is router-relative: the /admin basepath is already stripped.
  return DESTINATIONS.find((d) => pathname === `/${d.key}` || pathname.startsWith(`/${d.key}/`));
}
