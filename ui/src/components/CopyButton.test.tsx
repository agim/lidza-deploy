import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { CopyButton } from './ui';

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

describe('CopyButton', () => {
  it('copies exact multiline output without submitting an enclosing form', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    const submit = vi.fn(event => event.preventDefault());
    const value = 'compiler: <literal>\nsecond line\n';
    render(<form onSubmit={submit}><CopyButton value={value} label="Copy build log" /></form>);
    fireEvent.click(screen.getByRole('button', { name: 'Copy build log' }));
    await screen.findByRole('button', { name: 'Copied' });
    expect(writeText).toHaveBeenCalledWith(value);
    expect(submit).not.toHaveBeenCalled();
  });

  it('reports clipboard denial and permits retry', async () => {
    const writeText = vi.fn().mockRejectedValueOnce(new Error('permission denied')).mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    render(<CopyButton value="runtime output" label="Copy runtime log" />);
    fireEvent.click(screen.getByRole('button', { name: 'Copy runtime log' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not copy');
    expect(screen.queryByRole('button', { name: 'Copied' })).not.toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: 'Copy runtime log' })).not.toBeDisabled(), { timeout: 3000 });
    fireEvent.click(screen.getByRole('button', { name: 'Copy runtime log' }));
    await screen.findByRole('button', { name: 'Copied' });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});
