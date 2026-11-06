// State, validation and request building for the new-application wizard.
// Pure functions only, so the page stays a thin view over them.
import type { DefaultsRuntime } from '../../lib/defaults';
import type { ContainerRuntime, DeploymentMode, NativeCreateRequest, NativeRuntime, RepositoryDetection } from '../../lib/types';

export type DatabaseChoice = 'local' | 'external' | 'none';
export interface ProcessRow { key: string; name: string; command: string; web: boolean }
export interface DomainRow { key: string; value: string }
export interface EnvRow { key: string; name: string; value: string; kind: 'setting' | 'credential'; origin?: 'default'; edited?: boolean }

export interface WizardDraft {
  /** Set when the repository needs a container stack but has no stack manifest. */
  stackManifestMissing?: boolean;
  id: string;
  repository: string;
  installation: number;
  branch: string;
  autoDeploy: boolean;
  mode: DeploymentMode;
  nativeRuntime: NativeRuntime;
  containerRuntime: ContainerRuntime;
  framework: string;
  sourceDirectory: string;
  staticOutput: string;
  healthPath: string;
  processes: ProcessRow[];
  runtimeProfile: string;
  customerHosts: boolean;
  astroMode: 'static' | 'node';
  astroNode: string;
  serverID: string;
  domain: string;
  additionalDomains: DomainRow[];
  database: DatabaseChoice;
  databaseURL: string;
  redis: boolean;
  // clickhouse provisions a ClickHouse database on the server (native) or
  // the managed ClickHouse service (containers).
  clickhouse: boolean;
  backupHours: string;
  offsite: boolean;
  masterKey: string;
  env: EnvRow[];
}

export const steps = ['Name', 'Repository', 'Setup', 'Server', 'Domain', 'Database', 'Environment', 'Review'] as const;
export const lastStep = steps.length - 1;

let sequence = 0;
export const rowKey = () => `row-${++sequence}`;

export function emptyDraft(serverID = ''): WizardDraft {
  return {
    id: '', repository: '', installation: 0, branch: '', autoDeploy: false,
    mode: 'native', nativeRuntime: 'go', containerRuntime: 'go', framework: '', sourceDirectory: '', staticOutput: 'dist', healthPath: '/',
    processes: [{ key: rowKey(), name: 'web', command: '', web: true }],
    runtimeProfile: '', customerHosts: false, astroMode: 'static', astroNode: '22',
    serverID, domain: '', additionalDomains: [], database: 'none', databaseURL: '', redis: false, clickhouse: false, backupHours: '24', offsite: false,
    masterKey: '', env: [],
  };
}

export const runtimeLabels: Record<NativeRuntime, string> = { go: 'Go', rails: 'Ruby on Rails', node: 'Node.js', static: 'Static site' };
export const containerRuntimeLabels: Record<ContainerRuntime, string> = { go: 'Līdza (Go) container', rails: 'Ruby on Rails container', astro: 'Astro container' };
const frameworkLabels: Record<string, string> = { lidza: 'Līdza', rails: 'Ruby on Rails', go: 'Go', astro: 'Astro', next: 'Next.js', nuxt: 'Nuxt', sveltekit: 'SvelteKit', remix: 'Remix', nest: 'NestJS', express: 'Express', fastify: 'Fastify', koa: 'Koa', hono: 'Hono', hapi: 'hapi', node: 'Node.js', vite: 'Vite', 'create-react-app': 'Create React App', parcel: 'Parcel', html: 'HTML' };
export const frameworkLabel = (framework?: string) => (framework && frameworkLabels[framework]) || framework || 'Unknown';
const pinLabels: Record<string, string> = { ruby: 'Ruby', bundler: 'Bundler', node: 'Node', go: 'Go', go_toolchain: 'Go toolchain', package_manager: 'Package manager' };
export const pinLabel = (key: string) => pinLabels[key] || key;

// The container runtime that matches a detected application.
export function containerRuntimeFor(detection?: Pick<RepositoryDetection, 'runtime' | 'framework'>): ContainerRuntime {
  if (detection?.runtime === 'rails') return 'rails';
  if (detection?.framework === 'astro') return 'astro';
  return 'go';
}

export function defaultProcesses(runtime: NativeRuntime, framework = ''): ProcessRow[] {
  if (runtime === 'static') return [];
  const command = runtime === 'rails' ? 'bin/rails server' : runtime === 'node' ? (framework === 'astro' ? 'node dist/server/entry.mjs' : 'npm start') : 'bin/app';
  return [{ key: rowKey(), name: 'web', command, web: true }];
}

export function defaultHealthPath(runtime: NativeRuntime, framework = '') {
  if (runtime === 'static') return '';
  if (runtime === 'rails') return '/up';
  return framework === 'lidza' ? '/healthz' : '/';
}

// applyDetection replaces the setup and data choices with a detection's.
export function applyDetection(draft: WizardDraft, detection: RepositoryDetection): WizardDraft {
  const runtime: NativeRuntime = detection.runtime || draft.nativeRuntime;
  const framework = detection.framework || '';
  const processes = detection.processes?.length ? detection.processes.map(process => ({ key: rowKey(), name: process.name, command: process.command, web: !!process.web })) : defaultProcesses(runtime, framework);
  return {
    ...draft,
    branch: detection.branch || draft.branch,
    installation: detection.installation_id || draft.installation,
    mode: detection.mode === 'container' ? 'container' : 'native',
    nativeRuntime: runtime,
    containerRuntime: containerRuntimeFor(detection),
    framework,
    sourceDirectory: detection.source_directory || '',
    staticOutput: detection.static_output || (runtime === 'static' ? 'dist' : draft.staticOutput),
    healthPath: detection.health_path ?? defaultHealthPath(runtime, framework),
    processes: runtime === 'static' ? [] : processes,
    astroMode: framework === 'astro' && runtime === 'node' ? 'node' : 'static',
    database: detection.needs_postgres ? 'local' : 'none',
    redis: !!detection.needs_redis,
    clickhouse: !!detection.needs_clickhouse,
    stackManifestMissing: !!detection.stack_manifest_missing,
  };
}

// changeNativeRuntime resets runtime-specific setup to that runtime's defaults.
export function changeNativeRuntime(draft: WizardDraft, runtime: NativeRuntime): WizardDraft {
  if (runtime === draft.nativeRuntime) return draft;
  const framework = runtime === 'go' && draft.framework === 'lidza' ? 'lidza' : '';
  return { ...draft, nativeRuntime: runtime, framework, processes: defaultProcesses(runtime, framework), healthPath: defaultHealthPath(runtime, framework), staticOutput: draft.staticOutput || 'dist' };
}

export function isLidza(draft: WizardDraft) {
  return draft.mode === 'container' ? draft.containerRuntime === 'go' : draft.nativeRuntime === 'go' && draft.framework === 'lidza';
}

export function isRails(draft: WizardDraft) {
  return draft.mode === 'container' ? draft.containerRuntime === 'rails' : draft.nativeRuntime === 'rails';
}

export function masterKeyName(draft: WizardDraft) {
  return isLidza(draft) ? 'LIDZA_MASTER_KEY' : isRails(draft) ? 'RAILS_MASTER_KEY' : '';
}

export function defaultsKind(draft: WizardDraft): DefaultsRuntime {
  if (draft.mode === 'container') return draft.containerRuntime;
  if (draft.nativeRuntime === 'go' || draft.nativeRuntime === 'rails') return draft.nativeRuntime;
  return draft.framework === 'astro' ? 'astro' : 'node';
}

const credentialName = /(KEY|SECRET|TOKEN|PASSWORD|PASS|CREDENTIALS?)$/;

// mergeDefaults drops untouched defaults of another runtime and adds the
// workspace defaults that are not set yet.
export function mergeDefaults(rows: EnvRow[], values: Record<string, string>): EnvRow[] {
  const kept = rows.filter(row => row.origin !== 'default' || row.edited);
  const present = new Set(kept.map(row => row.name));
  const added = Object.keys(values).filter(name => values[name] !== '' && !present.has(name)).sort().map(name => ({ key: rowKey(), name, value: values[name], kind: credentialName.test(name) ? 'credential' as const : 'setting' as const, origin: 'default' as const }));
  return [...kept, ...added];
}

const idPattern = /^[a-z][a-z0-9-]{0,47}$/;
const repositoryPattern = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const hostnamePattern = /^(?=.{1,253}$)([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;
const processPattern = /^[a-z][a-z0-9_-]{0,31}$/;
const envPattern = /^[A-Za-z_][A-Za-z0-9_]*$/;

function validBranch(branch: string) {
  return branch !== '' && branch.length <= 200 && !branch.startsWith('-') && !/[ ~^:?*[\\\r\n\t]/.test(branch) && !branch.includes('..') && !branch.includes('@{');
}

function relativeDirectory(value: string) {
  return value === '' || value === '.' || (/^[A-Za-z0-9_./-]+$/.test(value) && !value.startsWith('/') && !value.split('/').includes('..'));
}

export function idError(id: string, existing: string[]) {
  if (!id) return 'Enter an application name.';
  if (!/^[a-z]/.test(id)) return 'Start with a lowercase letter.';
  if (!idPattern.test(id)) return id.length > 48 ? 'Use at most 48 characters.' : 'Use lowercase letters, numbers and dashes only.';
  if (existing.includes(id)) return 'An application with this name already exists.';
  return '';
}

export type StepErrors = Record<string, string>;

export function stepErrors(step: number, draft: WizardDraft, existing: string[] = []): StepErrors {
  const errors: StepErrors = {};
  const set = (field: string, message: string) => { if (message && !errors[field]) errors[field] = message; };
  switch (steps[step]) {
    case 'Name':
      set('id', idError(draft.id, existing));
      break;
    case 'Repository':
      if (!repositoryPattern.test(draft.repository) || draft.repository.includes('..')) set('repository', 'Choose a repository or enter it as owner/name.');
      if (!validBranch(draft.branch)) set('branch', 'Choose a branch.');
      break;
    case 'Setup':
      if (!relativeDirectory(draft.sourceDirectory.trim())) set('source_directory', 'Use a directory inside the repository.');
      if (draft.mode === 'native' && draft.nativeRuntime === 'static') {
        if (!draft.staticOutput.trim() || !relativeDirectory(draft.staticOutput.trim())) set('static_output', 'Enter the build output directory inside the repository.');
      } else if (draft.mode === 'native') {
        if (!draft.processes.length) set('processes', 'Add a web process.');
        const names = new Set<string>();
        draft.processes.forEach((process, index) => {
          if (!processPattern.test(process.name)) set(`process-${index}-name`, 'Use a lowercase name such as web or worker.');
          else if (names.has(process.name)) set(`process-${index}-name`, 'Each process needs its own name.');
          names.add(process.name);
          if (!process.command.trim()) set(`process-${index}-command`, 'Enter the command that starts this process.');
          else if (/[\r\n]/.test(process.command)) set(`process-${index}-command`, 'Use a single-line command.');
        });
        if (draft.processes.filter(process => process.web).length !== 1) set('processes', 'Choose exactly one web process.');
      }
      if ((draft.mode === 'native' && draft.nativeRuntime !== 'static' || draft.mode === 'container' && draft.containerRuntime === 'rails') && draft.healthPath && (!draft.healthPath.startsWith('/') || draft.healthPath.startsWith('//') || /[?#\s]/.test(draft.healthPath))) set('health_path', 'Use a local path such as /up, without a query.');
      if (draft.mode === 'container' && draft.containerRuntime === 'rails' && !draft.healthPath) set('health_path', 'Enter the readiness path.');
      if (draft.mode === 'container' && draft.containerRuntime === 'astro' && !relativeDirectory(draft.staticOutput.trim() || 'dist')) set('static_output', 'Use a directory inside the repository.');
      if (draft.mode === 'container' && draft.stackManifestMissing) set('mode', 'These services deploy as a container stack, which needs a deploy/lidza.stack.json manifest in the repository. Add one, or deploy the application natively.');
      break;
    case 'Server':
      if (!draft.serverID) set('server_id', 'Choose a server.');
      break;
    case 'Domain': {
      if (!hostnamePattern.test(draft.domain)) set('domain', 'Enter a hostname such as app.example.com.');
      const seen = new Set([draft.domain]);
      draft.additionalDomains.forEach((row, index) => {
        if (!hostnamePattern.test(row.value)) set(`domain-${index}`, 'Enter a hostname such as www.example.com.');
        else if (seen.has(row.value)) set(`domain-${index}`, 'This hostname is already listed.');
        seen.add(row.value);
      });
      if (draft.additionalDomains.length > 20) set('additional_domains', 'Use at most 20 additional hostnames.');
      break;
    }
    case 'Database':
      if (draft.database === 'external' && !/^postgres(ql)?:\/\/[^\s]+$/.test(draft.databaseURL.trim())) set('database_url', 'Enter a postgres:// connection URL.');
      break;
    case 'Environment': {
      const keyName = masterKeyName(draft);
      if (keyName === 'LIDZA_MASTER_KEY' && draft.masterKey.trim() && !/^[a-fA-F0-9]{64}$/.test(draft.masterKey.trim())) set('master_key', 'Use the 64 hexadecimal characters of the existing key, or leave blank.');
      if (keyName === 'RAILS_MASTER_KEY' && /\s/.test(draft.masterKey.trim())) set('master_key', 'Paste the key without spaces.');
      const seen = new Set<string>();
      draft.env.forEach((row, index) => {
        if (!envPattern.test(row.name)) set(`env-${index}`, 'Use letters, numbers and underscores, starting with a letter.');
        else if (seen.has(row.name) || row.name === keyName && draft.masterKey.trim()) set(`env-${index}`, `${row.name} is set more than once.`);
        else if (row.name === 'DATABASE_URL' && draft.database !== 'none') set(`env-${index}`, 'The database settings supply DATABASE_URL.');
        else if (row.name === 'REDIS_URL' && draft.redis && draft.mode === 'native') set(`env-${index}`, 'The Redis setting supplies REDIS_URL.');
        else if (draft.clickhouse && clickhouseKeys.includes(row.name)) set(`env-${index}`, `The ClickHouse setting supplies ${row.name}.`);
        else if (/[\r\n]/.test(row.value)) set(`env-${index}`, 'Values must be a single line.');
        seen.add(row.name);
      });
      break;
    }
  }
  return errors;
}

function environment(draft: WizardDraft) {
  const env: Record<string, string> = {};
  const kinds: Record<string, string> = {};
  for (const row of draft.env) { env[row.name] = row.value; kinds[row.name] = row.kind; }
  const keyName = masterKeyName(draft);
  if (keyName && draft.masterKey.trim()) { env[keyName] = draft.masterKey.trim(); kinds[keyName] = 'credential'; }
  return { env, kinds };
}

// The managed ClickHouse connection; the setting supplies these variables.
export const clickhouseKeys = ['CLICKHOUSE_URL', 'CLICKHOUSE_DATABASE', 'CLICKHOUSE_USER', 'CLICKHOUSE_PASSWORD', 'CLICKHOUSE_MANAGED'];

// hasBackups reports whether the application gets a database the server
// backs up: PostgreSQL, or ClickHouse on a native server.
export const hasBackups = (draft: WizardDraft) => draft.database !== 'none' || draft.mode === 'native' && draft.clickhouse;
const backupHours = (draft: WizardDraft) => hasBackups(draft) ? Number(draft.backupHours || 0) : 0;

export function nativeRequest(draft: WizardDraft): NativeCreateRequest {
  const { env, kinds } = environment(draft);
  const isStatic = draft.nativeRuntime === 'static';
  return {
    id: draft.id, repository: draft.repository, branch: draft.branch, runtime: draft.nativeRuntime,
    source_directory: draft.sourceDirectory.trim().replace(/^\/+|\/+$/g, ''),
    ...(isStatic ? { static_output: draft.staticOutput.trim() } : { health_path: draft.healthPath, processes: draft.processes.map(process => ({ name: process.name, command: process.command.trim(), ...(process.web ? { web: true } : {}) })) }),
    domain: draft.domain, additional_domains: draft.additionalDomains.map(row => row.value),
    data: { postgres: draft.database === 'local', redis: draft.redis, clickhouse: draft.clickhouse, ...(draft.database === 'external' ? { external_database_url: draft.databaseURL.trim() } : {}), backup_hours: backupHours(draft), offsite: hasBackups(draft) && draft.offsite },
    env, env_kinds: kinds, auto_deploy: draft.autoDeploy, server_id: draft.serverID, github_installation: draft.installation || 0,
  };
}

// containerRequest is the existing POST control/apps body.
export function containerRequest(draft: WizardDraft) {
  const { env, kinds } = environment(draft);
  // The managed ClickHouse service starts with the first deployment.
  if (draft.clickhouse) { env.CLICKHOUSE_MANAGED = 'true'; kinds.CLICKHOUSE_MANAGED = 'setting'; }
  const runtime = draft.containerRuntime;
  return {
    runtime, build: { source_directory: draft.sourceDirectory.trim().replace(/^\/+|\/+$/g, '') },
    ...(runtime === 'astro' ? { astro: { mode: draft.astroMode, output_directory: draft.staticOutput.trim() || 'dist', node_major: draft.astroNode } } : {}),
    ...(runtime === 'rails' ? { rails: { health_path: draft.healthPath || '/up' } } : {}),
    runtime_profile: runtime === 'go' ? draft.runtimeProfile : '',
    id: draft.id, repository: draft.repository, branch: draft.branch, domain: draft.domain,
    ...(draft.additionalDomains.length ? { additional_domains: draft.additionalDomains.map(row => row.value) } : {}),
    server_id: draft.serverID, auto_deploy: draft.autoDeploy, customer_hosts: runtime === 'go' && draft.customerHosts,
    github_installation: draft.installation || 0, env, env_kinds: kinds,
    database: { mode: draft.database, ...(draft.database === 'external' ? { url: draft.databaseURL.trim() } : {}), backup: { hours: backupHours(draft), keep: 1, offsite: draft.database !== 'none' && draft.offsite } },
  };
}

// A detection for the ?demo=1 preview, without a GitHub connection.
export function demoDetection(repository: string, branch = 'main'): RepositoryDetection {
  const base = { repository, branch, default_branch: 'main', branches: ['main', 'release'], commit: 'a6b92fe', installation_id: 1, detected: true };
  if (repository.endsWith('workspace')) {
    return { ...base, mode: 'container', needs_postgres: false, needs_redis: false, compose_file: 'compose.yaml', services: [{ name: 'synapse', image: 'ghcr.io/element-hq/synapse', role: 'image' }, { name: 'postfix', image: 'boky/postfix', role: 'image' }, { name: 'livekit', image: 'livekit/livekit-server', role: 'image' }, { name: 'collabora', image: 'collabora/code', role: 'image' }, { name: 'app', build: true, role: 'source' }], reasons: ['compose.yaml defines 5 services (synapse, postfix, livekit, collabora, app) using third-party images: container'] };
  }
  return { ...base, mode: 'native', runtime: 'rails', framework: 'rails', framework_version: '8.0.1', processes: [{ name: 'web', command: 'bin/rails server', web: true }, { name: 'worker', command: 'bundle exec sidekiq' }], needs_postgres: true, needs_redis: true, pins: { ruby: '3.4.1', bundler: '2.6.2' }, health_path: '/up', reasons: ['Gemfile and config/application.rb: Ruby on Rails 8.0', 'Readiness check: /up (Rails health check)', 'sidekiq gem: a worker process runs bundle exec sidekiq', 'pg gem: PostgreSQL', 'sidekiq gem: Redis', 'Builds and runs natively under systemd'] };
}
