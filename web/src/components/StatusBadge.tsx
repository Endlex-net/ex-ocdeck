import { isTransitional } from '../types';

const STATUS_META: Record<string, { label: string; cls: string }> = {
  active: { label: 'active', cls: 'badge-active' },
  suspended: { label: 'suspended', cls: 'badge-suspended' },
  archived: { label: 'archived', cls: 'badge-archived' },
  creating: { label: 'creating', cls: 'badge-pending' },
  activating: { label: 'activating', cls: 'badge-pending' },
  suspending: { label: 'suspending', cls: 'badge-pending' },
  deleting: { label: 'deleting', cls: 'badge-pending' },
  creation_failed: { label: 'creation failed', cls: 'badge-failed' },
  deletion_failed: { label: 'deletion failed', cls: 'badge-failed' },
};

export function StatusBadge({ status }: { status: string }) {
  const meta = STATUS_META[status] ?? { label: status, cls: 'badge-archived' };
  return (
    <span className={`badge ${meta.cls}`}>
      {isTransitional(status) && <span className="spinner spinner-inline" aria-hidden />}
      {meta.label}
    </span>
  );
}
