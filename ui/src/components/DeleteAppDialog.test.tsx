import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { DeleteAppAction } from './DeleteAppDialog';

const api = vi.hoisted(() => ({ get: vi.fn(), del: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn() }));
vi.mock('../lib/api', () => api);

afterEach(() => { cleanup(); vi.clearAllMocks(); });

it('explains what is kept and deleted, shows the restore date and needs the id typed', async () => {
  api.del.mockResolvedValue({ id: 'archive-1' });
  const deleted = vi.fn();
  render(<DeleteAppAction app={{ id: 'shop' }} now={new Date('2026-11-06T12:00:00Z')} onDeleted={deleted}><button>Delete</button></DeleteAppAction>);
  fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
  const dialog = await screen.findByRole('alertdialog');
  expect(within(dialog).getAllByText(/February 4, 2027/).length).toBeGreaterThan(0);
  expect(within(dialog).getByText(/Kept until/)).toBeInTheDocument();
  expect(within(dialog).getByText(/Every backup of its databases, on the server and off-site/)).toBeInTheDocument();
  expect(within(dialog).getByText(/with their containers stopped/)).toBeInTheDocument();
  expect(within(dialog).getByText(/hostnames become free at once/)).toBeInTheDocument();
  const submit = within(dialog).getByRole('button', { name: 'Delete application' });
  expect(submit).toBeDisabled();
  fireEvent.change(within(dialog).getByLabelText(/to delete it/), { target: { value: 'sho' } });
  expect(submit).toBeDisabled();
  fireEvent.change(within(dialog).getByLabelText(/Reason/), { target: { value: 'replaced' } });
  fireEvent.change(within(dialog).getByLabelText(/to delete it/), { target: { value: 'shop' } });
  fireEvent.click(submit);
  await waitFor(() => expect(api.del).toHaveBeenCalledWith('control/apps/shop', { confirm: 'shop', reason: 'replaced' }));
  await waitFor(() => expect(deleted).toHaveBeenCalledWith('archive-1'));
});

it('describes native applications with their own inventory', async () => {
  render(<DeleteAppAction app={{ id: 'blog', native: {} }} onDeleted={() => {}}><button>Delete</button></DeleteAppAction>);
  fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
  const dialog = await screen.findByRole('alertdialog');
  expect(within(dialog).getByText(/logins disabled/)).toBeInTheDocument();
  expect(within(dialog).getByText(/Systemd units, the system user/)).toBeInTheDocument();
  expect(within(dialog).getByText(/Redis instance and its data/)).toBeInTheDocument();
});
