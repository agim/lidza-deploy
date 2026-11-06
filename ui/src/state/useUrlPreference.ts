import { useEffect } from 'react';
import { setSearchParams, useSearchParam } from '../lib/router';
import { type PreferenceKey, validPreference } from './DisplayPreferences';
import { usePreference } from './PreferencesContext';

/**
 * useUrlPreference keeps a display choice in the URL query parameter `param`.
 * A valid value in the URL wins (a shared or bookmarked link shows the same
 * view); otherwise the saved preference applies and is reflected in the URL.
 * Changes update both, replacing the history entry rather than adding one.
 */
export function useUrlPreference(key: PreferenceKey, param: string, fallback: string): [string, (value: string) => void] {
  const [saved, setSaved] = usePreference<string>(key, fallback);
  const fromURL = useSearchParam(param);
  const valid = fromURL !== undefined && validPreference(key, fromURL);
  const value = valid ? fromURL : saved;
  useEffect(() => {
    if (valid) return;
    const reflected = saved === fallback ? undefined : saved;
    if (fromURL !== reflected) setSearchParams({ [param]: reflected });
  }, [valid, fromURL, saved, fallback, param]);
  return [value, next => { setSaved(next); setSearchParams({ [param]: next === fallback ? undefined : next }); }];
}
