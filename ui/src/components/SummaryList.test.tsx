import { cleanup, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { SummaryList } from './SummaryList';

afterEach(cleanup);

describe('SummaryList', () => {
  it('renders label/value pairs as one dl with a row per item and skips empty items', () => {
    render(<SummaryList aria-label="Plan" items={[
      { label: 'Hostname', value: <code>deploy.example.com</code> },
      false,
      { label: 'DNS', value: '203.0.113.9, 2001:db8::9' },
      null,
      { label: <span>Custom <b>label</b></span>, value: 4, id: 'custom' },
    ]} />);
    const list = screen.getByLabelText('Plan');
    expect(list.tagName).toBe('DL');
    expect(list).toHaveClass('summary-list');
    const rows = [...list.children];
    expect(rows).toHaveLength(3);
    for (const row of rows) {
      expect(row.tagName).toBe('DIV');
      expect([...row.children].map(child => child.tagName)).toEqual(['DT', 'DD']);
    }
    expect(within(rows[0] as HTMLElement).getByText('deploy.example.com').tagName).toBe('CODE');
    expect(rows[1]).toHaveTextContent('DNS203.0.113.9, 2001:db8::9');
    expect(rows[2]).toHaveTextContent('Custom label4');
  });

  it('renders nothing without items and keeps an extra class', () => {
    const { container, rerender } = render(<SummaryList items={[false, undefined]} />);
    expect(container.querySelector('dl')).toBeNull();
    rerender(<SummaryList className="extra" items={[{ label: 'A', value: 'B' }]} />);
    expect(container.querySelector('dl')).toHaveClass('summary-list', 'extra');
  });
});
