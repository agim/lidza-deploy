import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { useState } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { PromptDialog } from './ui';

function DialogHarness({ onSubmit }: { onSubmit: (value: string) => Promise<void> }) {
  const [open, setOpen] = useState(true);
  const [value, setValue] = useState('');
  return <PromptDialog open={open} title="Verify protected action" description="Enter a fresh code." label="Authenticator code" value={value} submitLabel="Verify" onValueChange={setValue} onOpenChange={setOpen} onSubmit={onSubmit} />;
}

describe('PromptDialog', () => {
  it('submits the entered value and closes after success', async () => {
    const submit = vi.fn(async () => undefined);
    render(<DialogHarness onSubmit={submit} />);

    fireEvent.change(screen.getByLabelText('Authenticator code'), { target: { value: ' 123456 ' } });
    fireEvent.click(screen.getByRole('button', { name: 'Verify' }));

    await waitFor(() => expect(submit).toHaveBeenCalledWith('123456'));
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument());
  });

  it('keeps the dialog open and shows an API error', async () => {
    render(<DialogHarness onSubmit={async () => { throw new Error('That code is no longer valid.'); }} />);

    fireEvent.change(screen.getByLabelText('Authenticator code'), { target: { value: '123456' } });
    fireEvent.click(screen.getByRole('button', { name: 'Verify' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('That code is no longer valid.');
    expect(screen.getByRole('alertdialog')).toBeInTheDocument();
  });
});
