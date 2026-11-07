import { ArrowLeft, ArrowRight, Boxes, Check, ChevronRight, Cpu, Database, GitBranch, Github, Globe, KeyRound, LoaderCircle, Plus, Rocket, Search, Server as ServerIcon, ShieldCheck, Tag, X } from 'lucide-react';
import { detailHref, goBack, href, navigate, previousEntry, searchHref, useSearchParam, withSticky } from '../lib/router';
import { type FormEvent, type ReactNode, useEffect, useMemo, useRef, useState } from 'react';
import { RuntimeProfileField } from '../components/RuntimeProfileField';
import { useToast } from '../components/ToastProvider';
import { Badge, Button, Card, ErrorBanner, Field, Notice, PageHeader } from '../components/ui';
import { Input } from '../components/primitives/input';
import { Switch } from '../components/primitives/switch';
import { type DatabaseChoice, type EnvRow, type StepErrors, type WizardDraft, applyDetection, changeNativeRuntime, containerRequest, containerRuntimeLabels, defaultsKind, demoDetection, emptyDraft, frameworkLabel, hasBackups, idError, isLidza, lastStep, masterKeyName, mergeDefaults, nativeRequest, pinLabel, rowKey, runtimeLabels, stepErrors, steps } from '../components/wizard/model';
import { get, post } from '../lib/api';
import { type DefaultsResponse, defaultsRuntimeLabels, runtimeProfile } from '../lib/defaults';
import type { ContainerRuntime, NativeRuntime, RepositoryDetection, ServerHealth, ServerHealthResponse } from '../lib/types';
import { assessBuildMemory } from '../lib/buildMemory';
import { useWorkspace } from '../state/WorkspaceContext';

type Repo = { full_name: string; default_branch?: string; private?: boolean; installation_id?: number };
type Detection = { state: 'idle' | 'loading' | 'done' | 'error'; result?: RepositoryDetection; error?: string };
type Created = { id: string; mode: WizardDraft['mode']; warning?: string };

const demoRepos: Repo[] = [{ full_name: 'acme/storefront', default_branch: 'main', private: true, installation_id: 1 }, { full_name: 'acme/workspace', default_branch: 'main', private: true, installation_id: 1 }];
const stepSlugs: string[] = steps.map(label => label.toLowerCase());
const stepSlug = (index: number) => stepSlugs[index] || stepSlugs[0];
const stepIndex = (slug?: string) => Math.max(0, stepSlugs.indexOf(slug || ''));
const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);

export function NewApplicationPage({ serverID }: { serverID?: string }) {
  const { servers, apps, status, refresh, loading } = useWorkspace();
  const demo = new URLSearchParams(location.search).get('demo') === '1';
  const notify = useToast();
  const [draft, setDraft] = useState<WizardDraft>(() => emptyDraft(serverID || ''));
  // Each step is its own history entry (?step=repository …), so the browser
  // Back button returns to the previous step. Steps beyond the furthest one
  // reached in this draft (a reload or a stale entry) fall back to it.
  const [reached, setReached] = useState(0);
  const reachedRef = useRef(0);
  const stepParam = useSearchParam('step');
  const routed = stepIndex(stepParam);
  const step = Math.min(routed, Math.max(reached, reachedRef.current));
  const stepURL = (target: number) => searchHref({ step: target === 0 ? undefined : stepSlug(target) });
  const setStep = (target: number) => navigate(stepURL(target));
  useEffect(() => { if (stepIndex(stepParam) !== step || (stepParam !== undefined && step === 0)) navigate(stepURL(step), { replace: true }); });
  const [attempted, setAttempted] = useState<Set<number>>(new Set());
  const [error, setError] = useState('');
  const [repos, setRepos] = useState<Repo[]>();
  const [reposError, setReposError] = useState('');
  const [repoFilter, setRepoFilter] = useState('');
  const [detection, setDetection] = useState<Detection>({ state: 'idle' });
  const [defaults, setDefaults] = useState<{ kind?: string; state: 'idle' | 'loading' | 'done' | 'error' }>({ state: 'idle' });
  const [created, setCreated] = useState<Created>();
  const [busy, setBusy] = useState(false);
  const detectRequest = useRef(0);
  const existing = useMemo(() => (apps || []).map(app => app.id), [apps]);
  const [health, setHealth] = useState<ServerHealth[]>();
  // The Server step compares the detected build's memory with each server's.
  const memory = assessBuildMemory(detection.result?.build_memory, health?.find(item => item.server_id === draft.serverID), servers.find(item => item.id === draft.serverID)?.name);
  const memoryBlocker = memory?.level === 'blocker' ? memory.message : undefined;
  const errorsAt = (index: number): StepErrors => ({ ...stepErrors(index, draft, existing), ...(index === 3 && memoryBlocker ? { server_id: memoryBlocker } : {}) });
  const errors = errorsAt(step);
  const shown = (field: string) => attempted.has(step) || (field === 'id' && draft.id !== '') ? errors[field] : undefined;
  const update = (patch: Partial<WizardDraft>) => setDraft(current => ({ ...current, ...patch }));
  const server = servers.find(item => item.id === draft.serverID);

  useEffect(() => { if (!loading && !servers.length && !demo) navigate(href('servers'), { replace: true }); }, [servers.length, loading, demo]);
  useEffect(() => { if (!draft.serverID && servers.length) update({ serverID: serverID && servers.some(item => item.id === serverID) ? serverID : servers[0].id }); }, [servers, serverID, draft.serverID]);

  // Authorized repositories load when the repository step opens.
  useEffect(() => {
    if (step !== 1 || repos) return;
    if (demo) { setRepos(demoRepos); return; }
    get<Repo[]>('control/github/repos').then(rows => setRepos([...rows].sort((a, b) => a.full_name.localeCompare(b.full_name)))).catch(reason => { setRepos([]); setReposError(message(reason)); });
  }, [step, repos, demo]);

  // Server memory loads when the server step opens.
  useEffect(() => {
    if (step !== 3 || health || demo) return;
    get<ServerHealthResponse>('control/server-health').then(response => setHealth(response.servers || [])).catch(() => setHealth([]));
  }, [step, health, demo]);

  // Workspace defaults of the chosen runtime prefill the environment once.
  const kind = defaultsKind(draft);
  useEffect(() => {
    if (step !== 6 || defaults.kind === kind || demo) return;
    setDefaults({ kind, state: 'loading' });
    get<DefaultsResponse>('control/application-defaults').then(response => {
      const values = runtimeProfile(response, kind).values || {};
      setDraft(current => ({ ...current, env: mergeDefaults(current.env, values) }));
      setDefaults({ kind, state: 'done' });
    }).catch(() => setDefaults({ kind, state: 'error' }));
  }, [step, kind, defaults.kind, demo]);

  const detect = async (repository: string, branch: string, installation: number, directory = '') => {
    const request = ++detectRequest.current;
    setDetection({ state: 'loading' });
    try {
      const params = new URLSearchParams({ repository, installation: String(installation || 0) });
      if (branch) params.set('branch', branch);
      if (directory) params.set('directory', directory);
      const result = demo ? demoDetection(repository, branch || 'main') : await get<RepositoryDetection>(`control/repos/detect?${params}`);
      if (request !== detectRequest.current) return;
      setDetection({ state: 'done', result });
      setDraft(current => applyDetection(current, result));
    } catch (reason) {
      if (request === detectRequest.current) setDetection({ state: 'error', error: message(reason) });
    }
  };
  const chooseRepo = (repo: Repo) => {
    update({ repository: repo.full_name, installation: repo.installation_id || 0, branch: repo.default_branch || '' });
    void detect(repo.full_name, repo.default_branch || '', repo.installation_id || 0);
  };
  const chooseBranch = (branch: string) => {
    update({ branch });
    if (draft.repository) void detect(draft.repository, branch, draft.installation);
  };

  const go = (target: number) => { setError(''); reachedRef.current = Math.max(reachedRef.current, target); setReached(reachedRef.current); setStep(target); };
  // The wizard's Back matches the browser Back button when that is the previous step.
  const back = () => { setError(''); if (previousEntry() === stepURL(step - 1)) history.back(); else setStep(step - 1); };
  const focusFirst = (found: StepErrors) => requestAnimationFrame(() => document.getElementById(`wizard-${Object.keys(found)[0]}`)?.focus());
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setAttempted(current => new Set(current).add(step));
    if (Object.keys(errors).length) { focusFirst(errors); return; }
    if (step === 1 && detection.state !== 'loading' && (detection.result?.repository !== draft.repository || detection.result?.branch !== draft.branch)) void detect(draft.repository, draft.branch, draft.installation);
    if (step < lastStep) { go(step + 1); return; }
    for (let index = 0; index < lastStep; index++) {
      const found = errorsAt(index);
      if (Object.keys(found).length) { setAttempted(current => new Set(current).add(index)); setStep(index); focusFirst(found); return; }
    }
    if (busy) return;
    setError('');
    setBusy(true);
    try {
      let warning = '';
      if (!demo) {
        const reply = draft.mode === 'native' ? await post<{ warning?: string }>('control/native-apps', nativeRequest(draft)) : await post<{ warning?: string }>('control/apps', containerRequest(draft));
        warning = reply?.warning || '';
      }
      setCreated({ id: draft.id, mode: draft.mode, warning });
      notify(`${draft.id} created.`);
      await refresh();
    } catch (reason) { setError(message(reason)); } finally { setBusy(false); }
  };
  const deployNow = async () => {
    if (!created) return;
    try {
      if (!demo) await post(`control/apps/${encodeURIComponent(created.id)}/deploy`);
      notify(`Deployment of ${created.id} started.`);
      navigate(href('deployments'));
    } catch (reason) { setError(message(reason)); }
  };

  if (created) return <div className="page form-page"><PageHeader eyebrow="Create application" title={`${created.id} is ready`} description="The application is registered. Deploy its first release now, or later from Applications." back={href('applications')} />
    <Card className="form-card wizard-done">{error && <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner>}
      <div className="section-title"><Check size={21} /><div><h2>Application created</h2><p>{created.mode === 'native' ? 'Its account, services and route are prepared on the server.' : 'Its container configuration is saved on the server.'}</p></div></div>
      {created.warning && <Notice>{created.warning}</Notice>}
      <div className="wizard-done-actions"><Button variant="primary" onClick={deployNow}><Rocket size={15} />Deploy now</Button><a className="button button-secondary" href={withSticky(detailHref('settings', created.id))}>Open settings</a><a className="button button-ghost" href={withSticky(href('applications'))}>Back to applications</a></div>
    </Card></div>;

  const result = detection.result;
  return <div className="page form-page"><PageHeader eyebrow="Create application" title="A new home for your code" description="Name it. Point it at a repository. Review what was detected. Make it live." back={href('applications')} />
    <nav className="stepper wizard-stepper" aria-label="Application setup progress"><ol>{steps.map((label, index) => { const done = index !== step && (index < step || index < reached); return <li key={label}><button type="button" disabled={index > reached} className={`step ${index === step ? 'current' : done ? 'complete' : ''}`} aria-current={index === step ? 'step' : undefined} onClick={() => go(index)}><span>{done ? <Check size={13} aria-label="Complete" /> : String(index + 1).padStart(2, '0')}</span><b className="wizard-step-label">{label}</b>{index < lastStep && <ChevronRight size={14} aria-hidden="true" />}</button></li>; })}</ol></nav>
    <form id="app-form" noValidate onSubmit={submit} aria-busy={busy || undefined}>{error && <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner>}
      <div className="wizard-panels">
        {step === 0 && <Panel icon={<Tag size={21} />} title="Application name" description="A short, permanent identifier used for the account, services and URLs.">
          <Field label="Application name" hint="Lowercase letters, numbers and dashes; up to 48 characters." error={shown('id')}><Input id="wizard-id" name="id" value={draft.id} onChange={event => update({ id: event.target.value.toLowerCase() })} placeholder="customer-portal" autoComplete="off" spellCheck={false} aria-invalid={!!shown('id') || undefined} autoFocus /></Field>
          {draft.id && !idError(draft.id, existing) && <p className="wizard-valid"><Check size={14} />{draft.id} is available.</p>}
        </Panel>}

        {step === 1 && <Panel icon={<Github size={21} />} title="Repository" description="Choose the code to deploy. The repository is inspected to propose its setup.">
          <div className="picker wizard-repos">
            <label className="search-field"><Search size={17} aria-hidden="true" /><span className="sr-only">Filter repositories</span><Input type="search" value={repoFilter} onChange={event => setRepoFilter(event.target.value)} placeholder="Filter authorized repositories…" aria-label="Filter repositories" /></label>
            {!repos && <p className="muted-copy"><LoaderCircle className="spin" size={15} /> Loading repositories…</p>}
            {reposError && <p className="muted-copy">{reposError} Enter the repository below instead.</p>}
            {repos && <div role="group" aria-label="Authorized repositories">{repos.filter(repo => repo.full_name.toLowerCase().includes(repoFilter.toLowerCase())).map(repo => <button key={repo.full_name} type="button" aria-pressed={draft.repository === repo.full_name} className={draft.repository === repo.full_name ? 'selected' : undefined} onClick={() => chooseRepo(repo)}><span>{repo.full_name}</span><small>{repo.private ? 'Private' : 'Public'}{repo.default_branch ? ` · ${repo.default_branch}` : ''}</small></button>)}{repos.length === 0 && !reposError && <p className="muted-copy">No repositories are authorized yet. Connect GitHub in Integrations.</p>}</div>}
          </div>
          <Field label="GitHub repository" hint="Or enter owner/name and detect it." error={shown('repository')}><div className="input-action"><Input id="wizard-repository" name="repository" value={draft.repository} onChange={event => update({ repository: event.target.value.trim(), installation: 0 })} placeholder="your-org/your-app" autoComplete="off" spellCheck={false} /><Button type="button" disabled={!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(draft.repository)} onClick={() => detect(draft.repository, draft.branch, draft.installation)}><Search size={16} />Detect</Button></div></Field>
          <Field label="Branch" hint="Deployments build this branch." error={shown('branch')}>{result?.branches?.length ? <select id="wizard-branch" name="branch" value={draft.branch} onChange={event => chooseBranch(event.target.value)}>{(result.branches.includes(draft.branch) || !draft.branch ? result.branches : [draft.branch, ...result.branches]).map(branch => <option key={branch} value={branch}>{branch}{branch === result.default_branch ? ' (default)' : ''}</option>)}</select> : <Input id="wizard-branch" name="branch" value={draft.branch} onChange={event => update({ branch: event.target.value.trim() })} placeholder="main" autoComplete="off" />}</Field>
          <DetectionStatus detection={detection} />
          <label className="check-row switch-row"><Switch checked={draft.autoDeploy} onCheckedChange={value => update({ autoDeploy: value })} aria-label="Deploy automatically on branch pushes" disabled={!status.github_app_connected || status.github_public_https === false} /><span><strong>Deploy automatically on branch pushes</strong><small>{status.github_app_connected ? 'Future pushes to this branch create releases.' : 'Connect GitHub in Integrations to enable this.'}</small></span></label>
        </Panel>}

        {step === 2 && <Panel icon={<Cpu size={21} />} title="Detected setup" description="Review how the application is built and run. Every value can be changed." badge={result?.detected ? <Badge tone="accent">{frameworkLabel(result.framework || (result.mode === 'container' ? 'Containers' : ''))}{result.framework_version ? ` ${result.framework_version}` : ''}</Badge> : undefined}>
          {detection.state === 'loading' && <p className="inline-guidance" role="status"><LoaderCircle className="spin" size={18} />Inspecting the repository…</p>}
          {detection.state === 'error' && <Notice>Detection was unavailable ({detection.error}). Choose the setup manually.</Notice>}
          {result && result.reasons.length > 0 && <div className="wizard-reasons"><strong>Why this setup</strong><ul>{result.reasons.map(reason => <li key={reason}>{reason}</li>)}</ul></div>}
          {result?.candidates?.length ? <div className="wizard-candidates"><strong>Choose the application directory</strong><div>{result.candidates.map(candidate => <Button key={candidate} type="button" size="sm" onClick={() => { update({ sourceDirectory: candidate }); void detect(draft.repository, draft.branch, draft.installation, candidate); }}>{candidate}/</Button>)}</div></div> : null}
          <fieldset className="wizard-choices"><legend className="field-label">Deployment</legend>
            <Choice name="mode" value="native" checked={draft.mode === 'native'} onChange={() => update({ mode: 'native' })} title="Native" description="Built on the server and run under systemd in its own account." />
            <Choice name="mode" value="container" checked={draft.mode === 'container'} onChange={() => update({ mode: 'container' })} title="Containers" description="Built and run with Docker; for repositories made of service images." />
          </fieldset>
          {shown('mode') && <p className="field-error" id="wizard-mode" role="alert">{shown('mode')}</p>}
          <div className="form-grid two">
            {draft.mode === 'native'
              ? <Field label="Runtime"><select id="wizard-runtime" value={draft.nativeRuntime} onChange={event => setDraft(current => changeNativeRuntime(current, event.target.value as NativeRuntime))}>{(Object.keys(runtimeLabels) as NativeRuntime[]).map(value => <option key={value} value={value}>{value === 'go' && draft.framework === 'lidza' ? 'Līdza (Go)' : runtimeLabels[value]}</option>)}</select></Field>
              : <Field label="Container runtime"><select id="wizard-runtime" value={draft.containerRuntime} onChange={event => update({ containerRuntime: event.target.value as ContainerRuntime, healthPath: event.target.value === 'rails' ? draft.healthPath || '/up' : draft.healthPath })}>{(Object.keys(containerRuntimeLabels) as ContainerRuntime[]).map(value => <option key={value} value={value}>{containerRuntimeLabels[value]}</option>)}</select></Field>}
            <Field label="Repository directory" hint="Leave blank when the app is at the repository root." error={shown('source_directory')}><Input id="wizard-source_directory" name="source_directory" value={draft.sourceDirectory} onChange={event => update({ sourceDirectory: event.target.value })} placeholder="apps/website" autoComplete="off" /></Field>
          </div>
          {draft.mode === 'native' && draft.nativeRuntime === 'static' && <Field label="Build output directory" hint="The directory the build writes and the web server publishes." error={shown('static_output')}><Input id="wizard-static_output" value={draft.staticOutput} onChange={event => update({ staticOutput: event.target.value })} placeholder="dist" /></Field>}
          {draft.mode === 'native' && draft.nativeRuntime !== 'static' && <ProcessList draft={draft} setDraft={setDraft} error={shown} />}
          {(draft.mode === 'native' && draft.nativeRuntime !== 'static' || draft.mode === 'container' && draft.containerRuntime === 'rails') && <Field label="Readiness path" hint="Returns HTTP 200 when the application can serve requests." error={shown('health_path')}><Input id="wizard-health_path" name="health_path" value={draft.healthPath} onChange={event => update({ healthPath: event.target.value.trim() })} placeholder="/up" maxLength={200} /></Field>}
          {draft.mode === 'container' && draft.containerRuntime === 'go' && <><RuntimeProfileField value={draft.runtimeProfile} onChange={event => update({ runtimeProfile: event.target.value })} /><label className="check-row switch-row"><Switch checked={draft.customerHosts} onCheckedChange={value => update({ customerHosts: value })} aria-label="Serve approved customer hostnames" /><span><strong>Serve approved customer hostnames</strong><small>Only for apps that validate additional hostnames through Līdza.</small></span></label></>}
          {draft.mode === 'container' && draft.containerRuntime === 'astro' && <div className="form-grid two"><Field label="Astro output"><select value={draft.astroMode} onChange={event => update({ astroMode: event.target.value as 'static' | 'node' })}><option value="static">Static website</option><option value="node">Standalone Node server</option></select></Field><Field label="Build output directory" error={shown('static_output')}><Input id="wizard-static_output" value={draft.staticOutput} onChange={event => update({ staticOutput: event.target.value })} placeholder="dist" /></Field><Field label="Node version"><select value={draft.astroNode} onChange={event => update({ astroNode: event.target.value })}><option value="22">Node 22</option><option value="24">Node 24</option></select></Field></div>}
          {result?.services?.length ? <div className="wizard-services"><strong>{result.compose_file || 'Compose'} services</strong><ul>{result.services.map(service => <li key={service.name}><code>{service.name}</code><span>{service.build ? 'Built from the repository' : service.image}</span><Badge tone={service.role === 'image' ? 'warning' : 'neutral'}>{service.role === 'image' ? 'Service image' : service.role === 'data' ? 'Data' : service.role === 'source' ? 'Source' : 'Development'}</Badge></li>)}</ul></div> : null}
          <div className="wizard-needs">
            <label className="check-row switch-row"><Switch checked={draft.database !== 'none'} onCheckedChange={value => update({ database: value ? (draft.database === 'external' ? 'external' : 'local') : 'none' })} aria-label="PostgreSQL database" /><span><strong>PostgreSQL database</strong><small>{result?.needs_postgres ? 'Detected from the repository.' : 'Not detected.'} Configure it in the Database step.</small></span></label>
            {draft.mode === 'native' && <label className="check-row switch-row"><Switch checked={draft.redis} onCheckedChange={value => update({ redis: value })} aria-label="Redis" /><span><strong>Redis</strong><small>{result?.needs_redis ? 'Detected from the repository.' : 'Not detected.'} A private Redis instance and REDIS_URL are created.</small></span></label>}
            <label className="check-row switch-row"><Switch checked={draft.clickhouse} onCheckedChange={value => update({ clickhouse: value })} aria-label="ClickHouse" /><span><strong>ClickHouse</strong><small>{result?.needs_clickhouse ? 'Detected from the repository.' : 'Not detected.'} Configure it in the Database step.</small></span></label>
          </div>
          {result?.pins && Object.keys(result.pins).length > 0 && <div className="wizard-pins"><strong>Toolchain versions from the repository</strong><ul>{Object.entries(result.pins).map(([key, value]) => <li key={key}><span>{pinLabel(key)}</span><code>{value}</code></li>)}</ul></div>}
        </Panel>}

        {step === 3 && <Panel icon={<ServerIcon size={21} />} title="Server" description="The server builds and runs the application.">
          <fieldset className="wizard-choices"><legend className="field-label">Hosting server</legend>{servers.map(item => <Choice key={item.id} id={draft.serverID === item.id ? 'wizard-server_id' : undefined} name="server_id" value={item.id} checked={draft.serverID === item.id} onChange={() => update({ serverID: item.id })} title={item.name} description={item.url} />)}</fieldset>
          {shown('server_id') && shown('server_id') !== memoryBlocker && <p className="field-error">{shown('server_id')}</p>}
          {memory && <p className={`build-memory-check build-memory-${memory.level}`} role={memory.level === 'ok' ? undefined : 'alert'}><Cpu size={18} /><span>{memory.message}{memory.estimate.basis.length > 0 && <small>Estimate: {memory.estimate.basis.join(', ')}.</small>}</span></p>}
          {draft.mode === 'container' ? <p className="inline-guidance"><Boxes size={18} />Container applications need Docker. If {server?.name || 'the server'} does not have it yet, Docker is installed and started automatically before the first deployment.</p> : <p className="inline-guidance"><ShieldCheck size={18} />Native applications run under systemd in their own system account. Docker is not required.</p>}
        </Panel>}

        {step === 4 && <Panel icon={<Globe size={21} />} title="Domain" description="Point the hostnames' A/AAAA records to the server. HTTPS certificates are issued and renewed automatically.">
          <Field label="Application hostname" error={shown('domain')}><Input id="wizard-domain" name="domain" value={draft.domain} onChange={event => update({ domain: event.target.value.trim().toLowerCase() })} placeholder="portal.example.com" autoComplete="off" spellCheck={false} /></Field>
          <div className="wizard-list"><span className="field-label">Additional hostnames</span>{draft.additionalDomains.map((row, index) => <div key={row.key} className="wizard-list-row"><Field label={`Additional hostname ${index + 1}`} error={shown(`domain-${index}`)}><Input id={`wizard-domain-${index}`} value={row.value} onChange={event => update({ additionalDomains: draft.additionalDomains.map(item => item.key === row.key ? { ...item, value: event.target.value.trim().toLowerCase() } : item) })} placeholder="www.example.com" autoComplete="off" /></Field><Button type="button" variant="ghost" size="icon" aria-label={`Remove additional hostname ${index + 1}`} onClick={() => update({ additionalDomains: draft.additionalDomains.filter(item => item.key !== row.key) })}><X size={17} /></Button></div>)}
            <Button type="button" size="sm" disabled={draft.additionalDomains.length >= 20} onClick={() => update({ additionalDomains: [...draft.additionalDomains, { key: rowKey(), value: '' }] })}><Plus size={15} />Add hostname</Button></div>
        </Panel>}

        {step === 5 && <Panel icon={<Database size={21} />} title="Database" description="Data services the application uses, and how they are backed up.">
          <fieldset className="wizard-choices"><legend className="field-label">PostgreSQL</legend>
            <Choice name="database_mode" value="local" checked={draft.database === 'local'} onChange={() => update({ database: 'local' })} title="Create PostgreSQL on the server" description="A private database and role; DATABASE_URL is set automatically." />
            <Choice name="database_mode" value="external" checked={draft.database === 'external'} onChange={() => update({ database: 'external' })} title="Use an existing database" description="Connect to a PostgreSQL server you already run." />
            <Choice name="database_mode" value="none" checked={draft.database === 'none'} onChange={() => update({ database: 'none' as DatabaseChoice })} title="No database" description="The application stores no relational data, or manages it itself." />
          </fieldset>
          {draft.database === 'external' && <Field label="PostgreSQL connection URL" hint="Use a server-reachable host and require TLS." error={shown('database_url')}><Input id="wizard-database_url" name="database_url" type="password" autoComplete="new-password" value={draft.databaseURL} onChange={event => update({ databaseURL: event.target.value })} placeholder="postgres://…?sslmode=require" /></Field>}
          {draft.mode === 'native' ? <label className="check-row switch-row"><Switch checked={draft.redis} onCheckedChange={value => update({ redis: value })} aria-label="Create Redis on the server" /><span><strong>Create Redis on the server</strong><small>A private Redis instance; REDIS_URL is set automatically.</small></span></label> : <p className="inline-guidance">Redis for container applications is defined by the repository's service configuration.</p>}
          <label className="check-row switch-row"><Switch checked={draft.clickhouse} onCheckedChange={value => update({ clickhouse: value })} aria-label="ClickHouse on this server" /><span><strong>ClickHouse on this server</strong><small>{result?.needs_clickhouse ? 'Detected from the repository. ' : ''}{draft.mode === 'native' ? 'A private ClickHouse database and user, backed up like PostgreSQL; ClickHouse is installed on first use.' : 'A private ClickHouse service started with the first deployment.'} CLICKHOUSE_URL, CLICKHOUSE_DATABASE, CLICKHOUSE_USER and CLICKHOUSE_PASSWORD are set automatically.</small></span></label>
          {hasBackups(draft) && <div className="backup-options"><div className="form-grid two"><Field label="Backup frequency"><select name="backup_hours" value={draft.backupHours} onChange={event => update({ backupHours: event.target.value })}><option value="0">Manual only</option><option value="1">Hourly</option><option value="24">Daily</option><option value="168">Weekly</option></select></Field></div><label className="check-row"><Input type="checkbox" checked={draft.offsite} onChange={event => update({ offsite: event.target.checked })} /><span><strong>Also copy to S3-compatible storage</strong><small>Requires verified workspace storage credentials.</small></span></label></div>}
        </Panel>}

        {step === 6 && <Panel icon={<KeyRound size={21} />} title="Environment" description="Values the application needs before its first deployment.">
          <MasterKeyField draft={draft} update={update} error={shown('master_key')} />
          <p className="inline-guidance"><KeyRound size={18} />{defaults.state === 'loading' ? 'Loading workspace defaults…' : defaults.state === 'error' ? 'Workspace defaults could not be loaded; they still apply on the first deployment.' : `${defaultsRuntimeLabels[kind]} workspace defaults are prefilled. Defaults also fill any variable left unset on the first deployment.`}</p>
          <EnvironmentList rows={draft.env} setRows={rows => update({ env: rows })} error={shown} />
        </Panel>}

        {step === 7 && <Panel icon={<Rocket size={21} />} title="Review" description="Check everything, then create the application.">
          <Review draft={draft} serverName={server?.name} />
        </Panel>}
      </div>
      <div className="wizard-actions"><Button type="button" variant="ghost" disabled={busy} onClick={() => step > 0 ? back() : goBack(href('applications'))}>{step > 0 ? <><ArrowLeft size={14} />Back</> : 'Cancel'}</Button><Button type="submit" variant="primary" pending={busy}>{step < lastStep ? <>Continue<ArrowRight size={15} /></> : <><Rocket size={15} />Create application</>}</Button></div>
    </form><p className="form-reassurance"><ShieldCheck size={14} />Your infrastructure. Your code. One place to deploy.</p></div>;
}

function Panel({ icon, title, description, badge, children }: { icon: ReactNode; title: string; description: string; badge?: ReactNode; children: ReactNode }) {
  return <Card className="form-card"><div className="section-title">{icon}<div><h2>{title}</h2><p>{description}</p></div>{badge}</div>{children}</Card>;
}

function Choice({ id, name, value, checked, onChange, title, description }: { id?: string; name: string; value: string; checked: boolean; onChange: () => void; title: string; description?: string }) {
  return <label className={`check-row wizard-choice${checked ? ' selected' : ''}`}><input id={id} type="radio" name={name} value={value} checked={checked} onChange={onChange} /><span><strong>{title}</strong>{description && <small>{description}</small>}</span></label>;
}

function DetectionStatus({ detection }: { detection: Detection }) {
  if (detection.state === 'loading') return <p className="inline-guidance" role="status"><LoaderCircle className="spin" size={18} />Inspecting the repository…</p>;
  if (detection.state === 'error') return <p className="inline-guidance" role="status">Detection unavailable: {detection.error} You can choose the setup manually in the next step.</p>;
  const result = detection.result;
  if (detection.state !== 'done' || !result) return null;
  if (!result.detected) return <p className="inline-guidance" role="status"><GitBranch size={18} />No supported application was recognised. Choose the setup in the next step.</p>;
  const what = result.mode === 'container' ? 'Containers' : `${frameworkLabel(result.framework)}${result.framework_version ? ` ${result.framework_version}` : ''}`;
  return <p className="inline-guidance" role="status"><Check size={18} />Detected {what}{result.source_directory ? ` in ${result.source_directory}/` : ''}{result.mode === 'native' ? ', deployed natively' : ''}.</p>;
}

function ProcessList({ draft, setDraft, error }: { draft: WizardDraft; setDraft: (update: (current: WizardDraft) => WizardDraft) => void; error: (field: string) => string | undefined }) {
  const change = (key: string, patch: Partial<WizardDraft['processes'][number]>) => setDraft(current => ({ ...current, processes: current.processes.map(process => process.key === key ? { ...process, ...patch } : patch.web ? { ...process, web: false } : process) }));
  return <fieldset className="wizard-list"><legend className="field-label">Processes</legend>
    {draft.processes.map((process, index) => <div className="wizard-process" key={process.key}>
      <Field label={`Process ${index + 1} name`} error={error(`process-${index}-name`)}><Input id={`wizard-process-${index}-name`} value={process.name} onChange={event => change(process.key, { name: event.target.value.trim() })} placeholder="worker" autoComplete="off" /></Field>
      <Field label={`Process ${index + 1} command`} error={error(`process-${index}-command`)}><Input id={`wizard-process-${index}-command`} value={process.command} onChange={event => change(process.key, { command: event.target.value })} placeholder="bundle exec sidekiq" autoComplete="off" spellCheck={false} /></Field>
      <label className="wizard-web"><input type="radio" name="web_process" checked={process.web} onChange={() => change(process.key, { web: true })} aria-label={`Process ${index + 1} serves web requests`} /><span>Web</span></label>
      <Button type="button" variant="ghost" size="icon" aria-label={`Remove process ${index + 1}`} disabled={draft.processes.length === 1} onClick={() => setDraft(current => { const rest = current.processes.filter(item => item.key !== process.key); return { ...current, processes: rest.some(item => item.web) || !rest.length ? rest : rest.map((item, position) => ({ ...item, web: position === 0 })) }; })}><X size={17} /></Button>
    </div>)}
    {error('processes') && <p className="field-error" id="wizard-processes" tabIndex={-1}>{error('processes')}</p>}
    <p className="field-hint">The web process listens on the port the server assigns in PORT. Commands run from the current release.</p>
    <Button type="button" size="sm" disabled={draft.processes.length >= 10} onClick={() => setDraft(current => ({ ...current, processes: [...current.processes, { key: rowKey(), name: '', command: '', web: current.processes.length === 0 }] }))}><Plus size={15} />Add process</Button>
  </fieldset>;
}

function MasterKeyField({ draft, update, error }: { draft: WizardDraft; update: (patch: Partial<WizardDraft>) => void; error?: string }) {
  const name = masterKeyName(draft);
  if (!name) return null;
  const lidza = isLidza(draft);
  return <Field label={lidza ? 'Application master key' : 'Rails master key'} hint={lidza ? 'Leave blank to generate a unique key securely on the server. Import the original LIDZA_MASTER_KEY when deploying existing sealed credentials or encrypted data.' : 'The contents of config/master.key (RAILS_MASTER_KEY), needed to read the encrypted credentials. Leave blank if the application does not use them.'} error={error}>
    <Input id="wizard-master_key" aria-label={lidza ? 'Application master key' : 'Rails master key'} type="password" autoComplete="new-password" value={draft.masterKey} onChange={event => update({ masterKey: event.target.value })} spellCheck={false} placeholder={lidza ? 'Generate automatically (recommended)' : 'Paste RAILS_MASTER_KEY'} />
  </Field>;
}

function EnvironmentList({ rows, setRows, error }: { rows: EnvRow[]; setRows: (rows: EnvRow[]) => void; error: (field: string) => string | undefined }) {
  const change = (key: string, patch: Partial<EnvRow>) => setRows(rows.map(row => row.key === key ? { ...row, ...patch, edited: true } : row));
  return <div className="wizard-list"><span className="field-label">Variables</span>
    {rows.length === 0 && <p className="muted-copy">No variables yet.</p>}
    <div className="simple-env-list">{rows.map((row, index) => <div key={row.key}>
      <Input id={`wizard-env-${index}`} aria-label={`Variable ${index + 1} name`} value={row.name} onChange={event => change(row.key, { name: event.target.value.trim() })} placeholder="ADMIN_USERS" autoComplete="off" spellCheck={false} aria-invalid={!!error(`env-${index}`) || undefined} />
      <select aria-label={`Variable ${index + 1} type`} value={row.kind} onChange={event => change(row.key, { kind: event.target.value as EnvRow['kind'] })}><option value="setting">Setting</option><option value="credential">Credential</option></select>
      <Input aria-label={`Variable ${index + 1} value`} type={row.kind === 'setting' ? 'text' : 'password'} value={row.value} onChange={event => change(row.key, { value: event.target.value })} autoComplete={row.kind === 'setting' ? 'off' : 'new-password'} placeholder="Value" />
      <Button type="button" variant="ghost" size="icon" aria-label={`Remove variable ${index + 1}`} onClick={() => setRows(rows.filter(item => item.key !== row.key))}><X size={17} /></Button>
      {(error(`env-${index}`) || row.origin === 'default' && !row.edited) && <small className={error(`env-${index}`) ? 'field-error wizard-env-note' : 'field-hint wizard-env-note'}>{error(`env-${index}`) || 'Workspace default'}</small>}
    </div>)}</div>
    <Button type="button" size="sm" onClick={() => setRows([...rows, { key: rowKey(), name: '', value: '', kind: 'credential' }])}><Plus size={15} />Add variable</Button>
  </div>;
}

function Review({ draft, serverName }: { draft: WizardDraft; serverName?: string }) {
  const native = draft.mode === 'native';
  const runtime = native ? (draft.nativeRuntime === 'go' && draft.framework === 'lidza' ? 'Līdza (Go)' : runtimeLabels[draft.nativeRuntime]) : containerRuntimeLabels[draft.containerRuntime];
  const keyName = masterKeyName(draft);
  const variables = [...draft.env.map(row => row.name), ...(keyName && draft.masterKey.trim() ? [keyName] : [])];
  const database = draft.database === 'local' ? 'PostgreSQL on the server' : draft.database === 'external' ? 'Existing PostgreSQL' : 'None';
  const backups = !hasBackups(draft) ? '—' : `${({ '0': 'Manual only', '1': 'Hourly', '24': 'Daily', '168': 'Weekly' } as Record<string, string>)[draft.backupHours] || `${draft.backupHours} h`}${draft.offsite ? ' · offsite copy' : ''}`;
  const rows: Array<[string, ReactNode]> = [
    ['Application', draft.id], ['Repository', `${draft.repository} @ ${draft.branch}`],
    ['Deployment', native ? 'Native (systemd)' : 'Containers (Docker)'], ['Runtime', `${runtime}${native && draft.framework && !['lidza', 'rails', 'go', draft.nativeRuntime].includes(draft.framework) ? ` · ${frameworkLabel(draft.framework)}` : ''}`],
    ['Repository directory', draft.sourceDirectory || 'Repository root'],
    ...(native && draft.nativeRuntime === 'static' ? [['Published directory', draft.staticOutput] as [string, ReactNode]] : []),
    ...(native && draft.nativeRuntime !== 'static' ? [['Processes', <ul className="wizard-review-list">{draft.processes.map(process => <li key={process.key}><code>{process.name}</code> {process.command}{process.web ? ' (web)' : ''}</li>)}</ul>] as [string, ReactNode], ['Readiness path', draft.healthPath || '—'] as [string, ReactNode]] : []),
    ['Server', serverName || draft.serverID], ['Hostnames', [draft.domain, ...draft.additionalDomains.map(row => row.value)].join(', ')],
    ['Database', database], ...(native ? [['Redis', draft.redis ? 'Redis on the server' : 'None'] as [string, ReactNode]] : []), ['ClickHouse', draft.clickhouse ? 'ClickHouse on the server' : 'None'], ['Backups', backups],
    ['Environment', variables.length ? variables.join(', ') : 'No variables'], ['Auto-deploy', draft.autoDeploy ? 'On branch pushes' : 'Manual'],
  ];
  return <dl className="review-box wizard-review">{rows.map(([label, value]) => <div key={label}><dt><span>{label}</span></dt><dd><strong>{value}</strong></dd></div>)}</dl>;
}
