import { afterEach, describe, expect, it, vi } from 'vitest';
import { APIError, get } from './api';

describe('api', () => {
  afterEach(() => { vi.unstubAllGlobals(); });

  it('answers undefined for 204 No Content', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(null, { status: 204 })));
    await expect(get('control/apps/portal/move')).resolves.toBeUndefined();
  });

  it('keeps 4xx responses as errors', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ error: 'unknown application' }), { status: 404 })));
    const error = await get('control/apps/missing/settings').catch((reason: APIError) => reason) as APIError;
    expect(error).toBeInstanceOf(APIError);
    expect(error.status).toBe(404);
    expect(error.message).toBe('unknown application');
  });
});
