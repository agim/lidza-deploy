import { describe, expect, it } from 'vitest';
import { parseRoute } from './router';

describe('console routes', () => {
  it('keeps workspace sections reloadable', () => {
    expect(parseRoute('#servers')).toEqual({ page: 'servers', id: undefined });
    expect(parseRoute('#remote-backups')).toEqual({ page: 'remote-backups', id: undefined });
  });

  it('preserves full-page resource routes', () => {
    expect(parseRoute('#page/settings/kulla-lidza')).toEqual({ page: 'detail', detail: 'settings', id: 'kulla-lidza' });
    expect(parseRoute('#page/console/local')).toEqual({ page: 'detail', detail: 'console', id: 'local' });
  });

  it('falls back safely for unknown links', () => {
    expect(parseRoute('#not-a-page')).toEqual({ page: 'applications' });
  });
});
