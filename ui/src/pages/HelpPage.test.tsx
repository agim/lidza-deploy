import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { ToastProvider } from '../components/ToastProvider';
import { helpArticles } from '../lib/help';
import { HelpPage } from './WorkspacePages';

afterEach(cleanup);

describe('HelpPage', () => {
  it('lists every article and filters them by the search', () => {
    const { container } = render(<ToastProvider><HelpPage /></ToastProvider>);
    expect(container.querySelectorAll('[data-help-article]')).toHaveLength(helpArticles.length);
    fireEvent.change(screen.getByLabelText('Search help'), { target: { value: 'redirect' } });
    expect(screen.getByRole('heading', { name: 'Redirect domains' })).toBeTruthy();
    expect(screen.queryByRole('heading', { name: 'Moving the control panel' })).toBeNull();
    fireEvent.change(screen.getByLabelText('Search help'), { target: { value: 'nothing matches this' } });
    expect(container.querySelectorAll('[data-help-article]')).toHaveLength(0);
    expect(screen.getByText('No matching guide')).toBeTruthy();
  });
});
