import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { SectionBoundary } from './SectionBoundary';

function Broken(): null { throw new Error('boom'); }

describe('SectionBoundary', () => {
  it('shows an error in place of a crashed section and keeps its siblings', () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    render(<div><SectionBoundary label="Server tuning"><Broken /></SectionBoundary><p>Still here</p></div>);
    expect(screen.getByText(/Server tuning could not be shown: boom/)).toBeTruthy();
    expect(screen.getByText('Still here')).toBeTruthy();
    spy.mockRestore();
  });
});
