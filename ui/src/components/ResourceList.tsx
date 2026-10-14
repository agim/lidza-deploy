import type { ReactNode } from 'react';

export interface Column<T> {
  id: string;
  label: string;
  render: (row: T) => ReactNode;
  className?: string;
}

export function ResourceList<T>({ rows, columns, rowKey, empty }: { rows: T[]; columns: Column<T>[]; rowKey: (row: T) => string; empty?: ReactNode }) {
  if (!rows.length) return <>{empty}</>;
  return <div className="resource-table" role="table"><div className="resource-table-head" role="row">{columns.map(column => <div key={column.id} role="columnheader" className={column.className}>{column.label}</div>)}</div><div className="resource-table-body">{rows.map(row => <div className="resource-row" role="row" key={rowKey(row)}>{columns.map(column => <div key={column.id} role="cell" data-label={column.label} className={column.className}>{column.render(row)}</div>)}</div>)}</div></div>;
}
