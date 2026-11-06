import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { BackLink, PageHeader } from '../components/ui';
import { canGoBack, goBack, historyIndex, navigate, normalizeLocation, previousEntry, routeLabel, setSearchParams, startRouter, useRoute } from './router';

function CurrentRoute() {
  const route = useRoute();
  return <p data-testid="route">{routeLabel(route)}</p>;
}
const here = () => location.pathname + location.search;

let stop: (() => void) | undefined;
beforeEach(() => { history.replaceState(null, '', '/console/applications'); normalizeLocation(); });
afterEach(() => { stop?.(); stop = undefined; cleanup(); vi.restoreAllMocks(); });

describe('history navigation', () => {
  it('pushes panel entries and marks them as in-panel history', () => {
    expect(canGoBack()).toBe(false);
    const before = history.length;
    act(() => navigate('/console/servers'));
    expect(here()).toBe('/console/servers');
    expect(history.length).toBe(before + 1);
    expect(historyIndex()).toBe(1);
    expect(canGoBack()).toBe(true);
    expect(previousEntry()).toBe('/console/applications');
    // Following a link to the shown view adds no entry.
    act(() => navigate('/console/servers'));
    expect(history.length).toBe(before + 1);
  });

  it('replaces the entry for query parameter changes', () => {
    const before = history.length;
    act(() => setSearchParams({ status: 'failed', q: 'portal' }));
    expect(here()).toBe('/console/applications?status=failed&q=portal');
    act(() => setSearchParams({ status: undefined }));
    expect(here()).toBe('/console/applications?q=portal');
    expect(history.length).toBe(before);
  });

  it('re-renders views on browser Back and Forward', async () => {
    render(<CurrentRoute />);
    expect(screen.getByTestId('route')).toHaveTextContent('Applications');
    act(() => navigate('/console/applications/portal/settings'));
    expect(screen.getByTestId('route')).toHaveTextContent('portal settings');
    act(() => history.back());
    await waitFor(() => expect(screen.getByTestId('route')).toHaveTextContent('Applications'));
    act(() => history.forward());
    await waitFor(() => expect(screen.getByTestId('route')).toHaveTextContent('portal settings'));
  });
});

describe('BackLink', () => {
  it('renders a real link to the parent view for new-tab use', () => {
    history.replaceState(null, '', '/console/servers/local/console?demo=1');
    render(<PageHeader title="Console" back="/console/servers" />);
    expect(screen.getByRole('link', { name: '← Back' })).toHaveAttribute('href', '/console/servers?demo=1');
  });

  it('opens the parent view when the panel has no earlier entry', () => {
    history.replaceState(null, '', '/console/servers/local/console');
    normalizeLocation();
    const back = vi.spyOn(history, 'back');
    render(<BackLink to="/console/servers" />);
    fireEvent.click(screen.getByRole('link', { name: '← Back' }));
    expect(back).not.toHaveBeenCalled();
    expect(here()).toBe('/console/servers');
  });

  it('lands where the browser Back button would', async () => {
    // Applications → Deployments → build logs: Back returns to Deployments, not the static parent.
    act(() => navigate('/console/deployments'));
    act(() => navigate('/console/applications/portal/logs'));
    render(<><CurrentRoute /><BackLink to="/console/applications" /></>);
    fireEvent.click(screen.getByRole('link', { name: '← Back' }));
    await waitFor(() => expect(here()).toBe('/console/deployments'));
    expect(screen.getByTestId('route')).toHaveTextContent('Deployments');
    expect(canGoBack()).toBe(true);
  });

  it('leaves modified clicks to the browser', () => {
    act(() => navigate('/console/servers/local'));
    const back = vi.spyOn(history, 'back');
    render(<BackLink to="/console/servers" />);
    const link = screen.getByRole('link', { name: '← Back' });
    const trap = (event: Event) => { expect(event.defaultPrevented).toBe(false); event.preventDefault(); };
    window.addEventListener('click', trap);
    for (const modifier of [{ ctrlKey: true }, { metaKey: true }, { shiftKey: true }, { button: 1 }]) fireEvent.click(link, modifier);
    window.removeEventListener('click', trap);
    expect(back).not.toHaveBeenCalled();
    expect(here()).toBe('/console/servers/local');
  });

  it('uses the shared Back for goBack callers', async () => {
    act(() => navigate('/console/servers'));
    goBack('/console/deployments');
    await waitFor(() => expect(here()).toBe('/console/applications'));
  });
});

describe('link interception', () => {
  it('handles in-panel links without a page load and keeps new-tab clicks native', () => {
    stop = startRouter();
    render(<><CurrentRoute /><a href="/console/servers">Servers</a><a href="/console/deployments" target="_blank" rel="noreferrer">New tab</a><a href="https://example.com/">External</a><a href="/login.html">Sign in</a></>);
    // Reports whether the router took the click, then stops jsdom's (unimplemented) page load.
    const routed = (element: HTMLElement, init: MouseEventInit = {}) => {
      let prevented = false;
      const trap = (event: Event) => { prevented = event.defaultPrevented; event.preventDefault(); };
      window.addEventListener('click', trap);
      act(() => { element.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, button: 0, ...init })); });
      window.removeEventListener('click', trap);
      return prevented;
    };
    expect(routed(screen.getByRole('link', { name: 'Servers' }))).toBe(true);
    expect(here()).toBe('/console/servers');
    expect(screen.getByTestId('route')).toHaveTextContent('Servers');
    for (const init of [{ ctrlKey: true }, { metaKey: true }, { shiftKey: true }, { button: 1 }]) expect(routed(screen.getByRole('link', { name: 'Servers' }), init)).toBe(false);
    expect(routed(screen.getByText('New tab'))).toBe(false);
    expect(routed(screen.getByText('External'))).toBe(false);
    expect(routed(screen.getByText('Sign in'))).toBe(false);
    expect(here()).toBe('/console/servers');
  });

  it('converts an old hash link followed inside the panel', async () => {
    stop = startRouter();
    render(<CurrentRoute />);
    act(() => { location.hash = 'page/settings/portal'; });
    await waitFor(() => expect(here()).toBe('/console/applications/portal/settings'));
    expect(location.hash).toBe('');
    expect(screen.getByTestId('route')).toHaveTextContent('portal settings');
    expect(canGoBack()).toBe(true);
  });
});
