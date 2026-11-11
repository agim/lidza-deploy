import { describe, expect, it } from 'vitest';
import { restartOutcome, saveLabel, settingsChanges, settingsEffect, settingsSavedMessage, type SettingsApplyEffects } from './settingsApply';
import type { AppSettings } from './types';

const effects: SettingsApplyEffects = { env: 'restart', build_env_prefixes: ['PUBLIC_'], fields: { branch: 'deploy', build: 'deploy', astro: 'deploy', runtime_profile: 'deploy', toolchains: 'deploy', domain: 'restart', additional_domains: 'immediate', customer_hosts: 'immediate', backup_before_deploy: 'immediate', env_kind: 'immediate', database_bindings: 'restart' } };
const base = { id: 'portal', branch: 'main', domain: 'portal.example.com', additional_domains: [], customer_hosts: false, backup_before_deploy: true } as unknown as AppSettings;

describe('settings apply', () => {
  it('names the edited fields and variables', () => {
    expect(settingsChanges(base, base)).toEqual([]);
    expect(settingsChanges(base, { ...base, branch: 'next', additional_domains: ['www.portal.example.com'] }, ['API_TOKEN'])).toEqual(['branch', 'additional_domains', 'env:API_TOKEN']);
  });

  it('reads Save and deploy only for a build-affecting change', () => {
    const label = (changes: string[]) => saveLabel(settingsEffect(effects, changes), 'Save changes');
    expect(label([])).toBe('Save changes');
    expect(label(['env:API_TOKEN'])).toBe('Save changes');
    expect(label(['additional_domains', 'backup_before_deploy'])).toBe('Save changes');
    expect(label(['env:PUBLIC_API_URL'])).toBe('Save and deploy');
    expect(label(['env:API_TOKEN', 'branch'])).toBe('Save and deploy');
    expect(label(['a_field_added_someday'])).toBe('Save and deploy');
    // A static site reads every variable at build time.
    expect(saveLabel(settingsEffect({ env: 'deploy', fields: {} }, ['env:TITLE']), 'Save variables')).toBe('Save and deploy');
    // An agent that sends no table keeps the plain label.
    expect(saveLabel(settingsEffect(undefined, ['branch']), 'Save changes')).toBe('Save changes');
  });

  it('says what a save applied', () => {
    expect(settingsSavedMessage({ applied: 'restart', deployment_id: 'r1', message: 'Settings saved. Restarting the application with them.' })).toBe('Settings saved. Restarting…');
    expect(settingsSavedMessage({ applied: 'deploy', deployment_id: 'd1', message: 'Settings saved. Deploying them.' })).toBe('Settings saved. Deploying…');
    expect(settingsSavedMessage({ applied: 'deploy', message: 'Settings saved, but the deployment could not start; deploy the application to apply them.', error: 'GitHub is not connected.' })).toBe('Settings saved, but the deployment could not start; deploy the application to apply them. GitHub is not connected.');
    expect(settingsSavedMessage({ applied: 'saved_restart_required', message: 'Settings saved. Restart the application to apply them; the panel cannot restart it: Service control is off for this server.' })).toContain('Service control is off');
    expect(settingsSavedMessage({ applied: 'immediate', message: 'Settings saved.' })).toBe('Settings saved.');
    expect(settingsSavedMessage(undefined)).toBe('Settings saved.');
    expect(restartOutcome({ id: 'r1', app_id: 'portal', status: 'live', created: '' })).toBe('Restarted.');
    expect(restartOutcome({ id: 'r1', app_id: 'portal', status: 'failed', created: '', error: 'myapp-server.service failed after restart; see its log' })).toBe('Restart failed: myapp-server.service failed after restart; see its log');
  });
});
