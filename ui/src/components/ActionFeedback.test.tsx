import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ActionForm, Button, ConfirmAction } from './ui';
import { ToastProvider } from './ToastProvider';
import { ACTION_FEEDBACK_MS } from './useActionFeedback';

beforeEach(() => { vi.useFakeTimers(); });
afterEach(() => { cleanup(); vi.clearAllTimers(); vi.useRealTimers(); });
const advance = async (milliseconds: number) => { await act(async () => { await vi.advanceTimersByTimeAsync(milliseconds); }); };

describe('shared action feedback', () => {
  it('acknowledges fast async actions for a minimum duration and blocks repeat clicks', async () => {
    const request = vi.fn(async () => undefined);
    render(<Button onClick={request}>Refresh</Button>);
    const button = screen.getByRole('button', { name: 'Refresh' });
    fireEvent.click(button);
    expect(request).toHaveBeenCalledTimes(1);
    expect(button).toHaveAttribute('aria-busy', 'true');
    expect(button.querySelector('.button-progress')).toBeInTheDocument();
    fireEvent.click(button);
    await advance(ACTION_FEEDBACK_MS - 1);
    expect(button).toBeDisabled();
    expect(request).toHaveBeenCalledTimes(1);
    await advance(1);
    expect(button).toBeEnabled();
    expect(button).not.toHaveAttribute('aria-busy');
  });

  it('keeps feedback active until a slow request actually settles', async () => {
    let resolve!: () => void;
    render(<Button onClick={() => new Promise<void>(finish => { resolve = finish; })}>Upload</Button>);
    const button = screen.getByRole('button', { name: 'Upload' });
    fireEvent.click(button);
    await advance(ACTION_FEEDBACK_MS * 2);
    expect(button).toBeDisabled();
    await act(async () => { resolve(); });
    expect(button).toBeEnabled();
  });

  it('tracks native form submissions, preserves currentTarget, and blocks repeated Enter submits', async () => {
    let formID = '';
    const submit = vi.fn(async (event) => { formID = event.currentTarget.id; });
    render(<ActionForm id="settings" onSubmit={submit}><input name="value" defaultValue="saved" /><Button type="submit">Save</Button></ActionForm>);
    const form = document.querySelector('form')!;
    fireEvent.submit(form);
    fireEvent.submit(form);
    expect(submit).toHaveBeenCalledTimes(1);
    expect(formID).toBe('settings');
    expect(screen.getByRole('button', { name: 'Save' })).toHaveAttribute('aria-busy', 'true');
    await advance(ACTION_FEEDBACK_MS);
    expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled();
  });

  it('does not add artificial busy feedback to navigation or state toggles', () => {
    const navigate = vi.fn();
    render(<Button onClick={navigate}>Settings</Button>);
    const button = screen.getByRole('button', { name: 'Settings' });
    fireEvent.click(button);
    expect(navigate).toHaveBeenCalledTimes(1);
    expect(button).not.toHaveAttribute('aria-busy');
  });

  it('surfaces rejected operations and leaves confirmation open for retry', async () => {
    render(<ToastProvider><ConfirmAction title="Restart server?" description="Restart the selected server." confirmLabel="Restart" onConfirm={async () => { throw new Error('Agent offline'); }}><Button>Open restart</Button></ConfirmAction></ToastProvider>);
    fireEvent.click(screen.getByRole('button', { name: 'Open restart' }));
    fireEvent.click(screen.getByRole('button', { name: 'Restart' }));
    await advance(0);
    expect(screen.getByText('Agent offline')).toBeInTheDocument();
    expect(screen.getByRole('alertdialog')).toBeInTheDocument();
    await advance(ACTION_FEEDBACK_MS);
    expect(screen.getByRole('button', { name: 'Restart' })).toBeEnabled();
  });
});
