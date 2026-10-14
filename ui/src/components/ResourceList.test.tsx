import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { ResourceList } from './ResourceList';

describe('ResourceList', () => {
  it('uses one column definition for desktop tables and mobile labels', () => {
    render(<ResourceList rows={[{ id: 'one', status: 'ready' }]} rowKey={row => row.id} columns={[{ id: 'id', label: 'Application', render: row => row.id }, { id: 'status', label: 'Status', render: row => row.status }]} />);
    expect(screen.getByRole('columnheader', { name: 'Application' })).toBeInTheDocument();
    expect(screen.getByRole('cell', { name: 'one' })).toHaveAttribute('data-label', 'Application');
    expect(screen.getByRole('cell', { name: 'ready' })).toHaveAttribute('data-label', 'Status');
  });
});
