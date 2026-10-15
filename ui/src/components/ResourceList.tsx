import { Table, TableHeader, TableBody, TableRow, TableHead, TableCell } from './primitives/table';
import type { ReactNode } from 'react';

export interface Column<T> {
  id: string;
  label: string;
  render: (row: T) => ReactNode;
  className?: string;
}

export function ResourceList<T>({ rows, columns, rowKey, empty }: { rows: T[]; columns: Column<T>[]; rowKey: (row: T) => string; empty?: ReactNode }) {
  if (!rows.length) return <>{empty}</>;
  return <Table className="resource-table"><TableHeader className="resource-table-head"><TableRow>{columns.map(column => <TableHead key={column.id} className={column.className}>{column.label}</TableHead>)}</TableRow></TableHeader><TableBody className="resource-table-body">{rows.map(row => <TableRow className="resource-row" key={rowKey(row)}>{columns.map(column => <TableCell key={column.id} data-label={column.label} className={column.className}>{column.render(row)}</TableCell>)}</TableRow>)}</TableBody></Table>;
}
