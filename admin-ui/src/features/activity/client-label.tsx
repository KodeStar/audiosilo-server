import {
  Globe,
  HelpCircle,
  KeyRound,
  LayoutDashboard,
  MonitorSmartphone,
  Smartphone,
  type LucideIcon,
} from 'lucide-react';
import type { ClientInfo } from '@/api/types';
import { clientKind, type ClientKind } from './activity-model';

// How the app behind a device or a session is named and drawn, the same on the
// Activity and People screens.

const ICONS: Record<ClientKind | 'api', LucideIcon> = {
  unknown: HelpCircle,
  console: LayoutDashboard,
  phone: Smartphone,
  web: Globe,
  other: MonitorSmartphone,
  api: KeyRound,
};

/** A device's icon in a soft tile: a phone, a browser, the console, an API key. */
export function ClientIcon({
  client,
  apiKey,
  size = 36,
}: {
  client: ClientInfo | null;
  apiKey?: boolean;
  size?: number;
}) {
  const Icon = ICONS[apiKey ? 'api' : clientKind(client)];
  return (
    <span
      className="grid shrink-0 place-items-center rounded-[10px] bg-muted text-muted-foreground"
      style={{ width: size, height: size }}
      aria-hidden="true"
    >
      <Icon className="size-[17px]" />
    </span>
  );
}
