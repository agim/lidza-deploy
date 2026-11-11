import { cleanup, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { CheckList, checkStatus } from './CheckList';

afterEach(cleanup);

describe('CheckList', () => {
  it('renders each item as icon, status for screen readers, then the text in one row', () => {
    render(<CheckList aria-label="Checks" items={[
      { id: 'a', status: 'ok', children: 'Same release.' },
      { id: 'b', status: 'warning', children: 'Agent unreachable.', detail: 'Rewritten to loopback.' },
      { id: 'c', status: 'failed', children: <span>Disk <code>/var</code> full.</span> },
    ]} />);
    const list = screen.getByRole('list', { name: 'Checks' });
    expect(list).toHaveClass('check-list');
    const items = within(list).getAllByRole('listitem');
    expect(items.map(item => item.getAttribute('data-status'))).toEqual(['ok', 'warning', 'failed']);
    for (const item of items) {
      const [icon, status, text] = [...item.children];
      expect(icon).toHaveClass('check-list-icon');
      expect(icon).toHaveAttribute('aria-hidden', 'true');
      expect(icon.querySelector('svg')).not.toBeNull();
      expect(status).toHaveClass('sr-only');
      expect(text).toHaveClass('check-list-text');
    }
    expect(items[0]).toHaveTextContent('Passed: Same release.');
    expect(items[1]).toHaveTextContent('Needs attention: Agent unreachable.Rewritten to loopback.');
    expect(within(items[1]).getByText('Rewritten to loopback.')).toHaveClass('check-list-detail');
    expect(items[2]).toHaveClass('check-list-failed');
  });

  it('spins the running icon, accepts a custom icon and renders nothing when empty', () => {
    const { container, rerender } = render(<CheckList items={[{ status: 'running', children: 'Copying' }, { status: 'info', icon: <i data-testid="gem" />, children: 'Ruby 3.3' }]} />);
    expect(container.querySelector('.check-list-running svg')).toHaveClass('spin');
    expect(screen.getByTestId('gem').closest('.check-list-icon')).not.toBeNull();
    rerender(<CheckList items={[]} />);
    expect(container.querySelector('ul')).toBeNull();
  });

  it('maps a boolean result to a status', () => {
    expect(checkStatus(true)).toBe('ok');
    expect(checkStatus(false)).toBe('warning');
    expect(checkStatus(false, 'waiting')).toBe('waiting');
  });
});
