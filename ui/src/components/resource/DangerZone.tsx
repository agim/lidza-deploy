import type { ReactNode } from 'react';
import { Card, SectionHeader } from '../ui';

/** DangerZone: the last section of a resource's settings tab, holding its destructive actions. */
export function DangerZone({ id, description, children }: { id: string; description: string; children: ReactNode }) {
  return <Card className="form-card danger-zone" data-danger-zone={id}><SectionHeader title="Danger zone" description={description} />{children}</Card>;
}

/** One destructive action: what it does on the left, its button on the right. */
export function DangerZoneRow({ title, description, note, action }: { title: string; description: ReactNode; note?: ReactNode; action: ReactNode }) {
  return <div className="danger-zone-row"><div><strong>{title}</strong><small>{description}</small>{note && <small role="note">{note}</small>}</div>{action}</div>;
}
