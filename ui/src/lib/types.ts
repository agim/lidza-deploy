export type ID = string;

export interface Server {
  id: ID;
  name: string;
  url: string;
}

export interface ServerHealth {
  server_id: ID;
  checked?: string;
  cpus?: number;
  cpu_percent?: number | null;
  load1?: number;
  memory_total?: number;
  memory_available?: number;
  swap_total?: number;
  swap_free?: number;
  swap_provisioning?: { state?: string; message?: string };
  disks?: Array<{ path: string; total?: number; available?: number; used_percent?: number }>;
  database_bytes?: Record<string, number>;
  /** Databases no application uses any more, with their last backup; one that still answers is also in database_bytes. */
  retained_databases?: Record<string, { backup_bytes?: number; backup_created?: string }>;
  errors?: string[];
  /** Host conditions to tidy up that do not stop deployments (for example stale Bundler versions). */
  warnings?: string[];
  /** Host conditions the agent already handles; nothing is required of the operator. */
  notes?: string[];
  /** A disk is at least 85% full or inside its free-space reserve: new backups, builds and move transfers are refused. */
  disk_low?: boolean;
  /** Disk space of each managed ClickHouse's system log tables, by application ("host" for the host server). */
  clickhouse_system_logs?: Record<string, number>;
  ingress?: HostIngressStatus;
  /** Present only when Docker is installed on the server. */
  docker?: { version?: string; containers: number; running: number };
  /** Native applications and systemd; present whenever the server has systemd. */
  native?: NativeHostHealth;
}

export interface NativeHostHealth {
  applications: number;
  units: number;
  active: number;
  inactive: number;
  failed_units: string[];
  failed_host_units: string[];
  journal_bytes?: number;
  releases: Array<{ app_id: string; path: string; bytes: number; releases: number; measured: string }>;
}

export interface ServerHealthResponse {
  servers: ServerHealth[];
  unavailable_servers?: string[];
}

export interface DomainStatus {
  domain?: string;
  checked?: string;
  state?: string;
  message?: string;
  recheck_seconds?: number;
}

/** A hostname that redirects (301 by default) to one of the application's hostnames or any other. */
export interface RedirectDomain {
  host: string;
  /** Empty: the application's primary hostname. */
  target?: string;
  status?: number;
  keep_path: boolean;
}

/** The redirect domains of an application, how its server serves them and the records their DNS needs. */
export interface RedirectDomainsView {
  redirects: RedirectDomain[];
  hostnames: string[];
  statuses?: DomainStatus[];
  ingress: 'agent' | 'caddy' | 'caddyfile' | string;
  detail: string;
  note?: string;
  server_id: string;
  addresses: string[];
  records: Array<{ hostname: string; type: string; value: string; note?: string }>;
  warnings: string[];
}

export interface Application {
 management_capabilities?: {deploy?:boolean;rollback?:boolean;configure?:boolean;environment?:boolean;remove?:boolean;
  /** The application can be stopped, started and restarted from the panel; lifecycle_reason says why not. */
  lifecycle?:boolean;lifecycle_reason?:string;
  /** A native application adopted from the existing host setup. */
  adopted?:boolean};
  native?: { id: string; name: string; services: string[]; runtime?: string; document_root?:string };
  observed?: { name: string; source: string; processes: string[]; observed_at: string; automatic?: boolean; missing?: boolean };
  runtime?: "go" | "rails" | "astro";
 astro?: {mode?: "static" | "node";output_directory?:string;node_major?:string;health_path?:string};
 build?: {source_directory?:string};
  rails?: { health_path?: string };
 runtime_profile?: string;
  id: ID;
  server_id: ID;
  repository: string;
  branch: string;
  domain: string;
  additional_domains?: string[];
  redirect_domains?: RedirectDomain[];
  domain_statuses?: DomainStatus[];
  auto_deploy?: boolean;
  customer_hosts?: boolean;
  stopped?: boolean;
  retiring?: boolean;
  preview_parent?: string;
  github_installation?: number;
  current?: { commit?: string; stack?: { bootstrap?: boolean; manifest?: { bootstrap_readiness?: { setup_path?: string } } } };
  previous?: { commit?: string };
  domain_status?: DomainStatus;
  agent_error?: string;
  /** Conditions of the current release to fix, such as a mail port without a certificate. */
  warnings?: string[];
  maintenance?: { enabled?: boolean };
  previews?: { enabled?: boolean; database?: boolean; base_domain?: string };
  /** The application's active move; locked during its final sync, when changes are refused until it finishes or is aborted. */
  move?: { id: string; state: string; source_server: string; target_server: string; locked?: boolean; phase?: string; step?: string; error?: string; retry?: string };
}

export interface Deployment {
  id: ID;
  app_id: ID;
  server_id?: ID;
  status: string;
  kind?: string;
  commit?: string;
  commit_message?: string;
  created: string;
  /** When the build started running; absent while queued. */
  started?: string;
  /** When the deployment last produced output. */
  updated?: string;
  /** Live phase while the deployment waits (for another build on the server, or for memory). */
  progress?: { phase: string; done_bytes: number; total_bytes?: number; started_at?: string; updated_at?: string };
  finished?: string;
  duration_seconds?: number;
  error?: string;
  log?: string;
}

export interface ControlStatus {
  native_inventory?: Array<{server_id: string; state: 'pending' | 'synced' | 'unsupported' | 'error'; checked?: string; message?: string; apps: number}>;
  roles?: string[];
  github_app_configured?: boolean;
  github_app_connected?: boolean;
  github_public_https?: boolean;
  github_app_slug?: string;
  team_available?: boolean;
  two_factor_enabled?: boolean;
  [key: string]: unknown;
}

export interface EnvironmentEntry {
  source?: string;
  key: string;
  kind?: 'setting' | 'credential' | 'auto';
  value?: string;
  preview?: string;
  length?: number;
}

export interface AppSettings extends Application {
 readonly_env_keys?:string[];
 settings_apply_on_deploy?:boolean;
 defaults_runtime?: 'go' | 'rails' | 'astro' | 'node';
 release_runtime?: string;
 build_configuration_supported?: boolean;
 capabilities?: {routing?:boolean;resources?:boolean;runtime_mode?:boolean;configure:boolean;environment:boolean;deploy:boolean;reload:boolean;rollback:boolean;processes:boolean;logs:boolean;reason?:string;remove?:boolean;lifecycle?:boolean;lifecycle_reason?:string;adopted?:boolean};
 public_ports_supported?: boolean;
 master_key_generation_supported?: boolean;
 stack_profiles_supported?: boolean;
 runtime_deployed?: boolean;
  additional_domains_supported?: boolean;
  backup_before_deploy?: boolean;
  env_keys?: string[];
  environment?: EnvironmentEntry[];
  env_sources?: Record<string, string>;
  database_bindings?: Record<string, string>;
  cache?: Record<string, unknown>;
  persistent_storage?: boolean;
  storage_provider_override?: boolean;
  app_managed_storage_supported?: boolean;
}

export interface ContainerInfo {
  public_tls?: { hostname: string; status: string; not_after?: string; fingerprint?: string; warning?: string };
  ports?: ForwardedPort[];
  id?: string;
  name: string;
  image?: string;
  status?: string;
  state?: string;
  health?: string;
  app_id?: string;
  service?: string;
  role?: string;
  cpu?: string;
  memory?: string;
  memory_percent?: string;
  network_io?: string;
  block_io?: string;
  pids?: string;
  restartable?: boolean;
  needs_attention?: boolean;
  cpu_percent?: number;
  memory_bytes?: number;
  memory_limit_bytes?: number;
  restart_count?: number;
  /** "native" for systemd units of native applications. */
  runtime?: string;
  unit?: string;
  since?: string;
  [key: string]: unknown;
}

export interface ContainerInventory {
  checked_at?: string;
  containers?: ContainerInfo[];
  warning?: string;
}

export interface WorkspaceData {
  apps: Application[];
  servers: Server[];
  deployments: Deployment[];
  status: ControlStatus;
  unavailableServers: string[];
}

export type PageKey =
  | 'applications' | 'deployments' | 'errors' | 'security' | 'servers'
  | 'databases' | 'remote-backups' | 'integrations' | 'defaults' | 'updates'
  | 'team' | 'audit' | 'help' | 'control-panel' | 'new-application' | 'deleted-apps' | 'detail' | 'not-found';

export interface Route {
  page: PageKey;
  detail?: string;
  id?: string;
  /** Selected tab of a tabbed detail view (application settings). */
  tab?: string;
}

export interface ForwardedPort {
 app_id: string; service: string; container: string; release: string;
 declared: boolean; protocol: string; host_ip: string; host_port: number;
 container_port: number; publication: string; firewall: string; external: string;
 bindings?: { HostIp: string; HostPort: string }[];
}

// Repository detection and native application creation (new-application wizard).
export type DeploymentMode = 'native' | 'container';
export type NativeRuntime = 'go' | 'rails' | 'node' | 'static';
export type ContainerRuntime = 'go' | 'rails' | 'astro';

export interface DetectedProcess {
  name: string;
  command: string;
  web?: boolean;
}

export interface ComposeService {
  name: string;
  image?: string;
  build?: boolean;
  role: 'data' | 'development' | 'source' | 'image';
}

export interface RepositoryDetection {
  repository: string;
  branch: string;
  default_branch: string;
  branches: string[];
  commit?: string;
  installation_id?: number;
  detected: boolean;
  mode: DeploymentMode;
  runtime?: NativeRuntime;
  framework?: string;
  framework_version?: string;
  source_directory?: string;
  processes?: DetectedProcess[];
  needs_postgres: boolean;
  needs_redis: boolean;
  needs_clickhouse?: boolean;
  pins?: Record<string, string>;
  health_path?: string;
  static_output?: string;
  dockerfile?: boolean;
  compose_file?: string;
  services?: ComposeService[];
  stack_manifest?: string;
  stack_services?: string[];
  stack_manifest_missing?: boolean;
  candidates?: string[];
  reasons: string[];
  /** Estimated memory of the build's largest step. */
  build_memory?: BuildMemoryEstimate;
}

/** The memory a build is expected to need, with the reasons. */
export interface BuildMemoryEstimate {
  mib: number;
  basis: string[];
  /** Measured on earlier builds rather than estimated from the stack. */
  learned?: boolean;
}

/** A build estimate compared with a server's memory. */
export interface BuildMemoryAssessment {
  estimate: BuildMemoryEstimate;
  total_mib?: number;
  available_mib?: number;
  reserve_mib?: number;
  level: 'ok' | 'warning' | 'blocker';
  message?: string;
}

export interface NativeCreateRequest {
  id: string;
  repository: string;
  branch: string;
  runtime: NativeRuntime;
  source_directory?: string;
  static_output?: string;
  domain: string;
  additional_domains?: string[];
  health_path?: string;
  processes?: DetectedProcess[];
  data: { postgres?: boolean; redis?: boolean; clickhouse?: boolean; external_database_url?: string; backup_hours?: number; offsite?: boolean };
  env?: Record<string, string>;
  env_kinds?: Record<string, string>;
  auto_deploy?: boolean;
  server_id: string;
  github_installation?: number;
  deploy?: boolean;
}

// Host management (agent host_updates.go / host_firewall.go).
export interface HostRebootWindow { weekdays?: number[]; hour: number; duration_minutes: number }
export interface HostUpdatesPolicy { auto_security: boolean; auto_reboot: boolean; reboot_window: HostRebootWindow; manage_package_sources: boolean; disruptive_in_window?: boolean }
export interface HostPackageUpgrade { name: string; current?: string; candidate: string; security: boolean; disruptive?: boolean; effect?: string }
export interface HostDockerStatus { installed: boolean; running: boolean; version?: string; live_restore: boolean; live_restore_configured: boolean; managed: boolean; restart_pending?: boolean; warning?: string }
export interface HostComponent { name: string; package?: string; installed?: string; candidate?: string; upgrade_available: boolean; source: 'package' | 'binary' | 'absent'; repository?: string; automatic: 'security' | 'all' | 'none' }
export interface HostJob { id: string; kind: string; trigger: string; status: 'running' | 'succeeded' | 'failed'; started: string; finished?: string; summary?: string; output?: string }
export interface HostUpdatesStatus {
  supported: boolean; reason?: string; checked_at?: string;
  pending: number; security: number; disruptive?: number; packages?: HostPackageUpgrade[]; disruptive_packages?: HostPackageUpgrade[];
  reboot_required: boolean; reboot_packages?: string[]; reboot_reason?: string;
  unattended_installed: boolean; unattended_enabled: boolean; unattended_last_run?: string;
  managed_origins?: string[]; components?: HostComponent[]; docker?: HostDockerStatus;
  policy: HostUpdatesPolicy; configured: boolean;
  next_reboot_window?: string; next_disruptive_window?: string; last_reboot?: string; reboot_deferred?: string;
  running?: string; jobs?: HostJob[]; error?: string;
}
export interface HostFirewallRule { port: string; proto: 'tcp' | 'udp' | 'any'; from?: string[]; comment?: string; source: string; enforced?: 'ufw' | 'docker' }
export interface HostFirewallModel { enabled: boolean; default_inbound: string; ssh_from?: string[]; rules: HostFirewallRule[] }
export interface HostFirewallView {
  supported: boolean; reason?: string; backend: string; installed: boolean; active: boolean; default_incoming?: string;
  model: HostFirewallModel; rules: HostFirewallRule[]; kept?: string[];
  pending_deadline?: string; pending_seconds?: number; applying?: boolean;
  last_applied?: string; last_error?: string; notes?: string[];
}
export interface HostIngressCaddy { admin: string; server?: string }
export interface HostIngressStatus {
  mode: 'agent' | 'caddy'; source: 'config' | 'setting' | 'detected'; caddy_admin?: string; caddy_server?: string;
  admin_api: boolean; web_ports?: number[]; detail?: string; checked: string; detected?: 'agent' | 'caddy';
  override?: HostIngressCaddy; configured: boolean; error_pages?: HostErrorPages;
}
export interface HostErrorPageRoot { server: string; root: string; exists: boolean; hosts: string[] }
export interface HostErrorPages { dir: string; ready: boolean; problem?: string; caddy_user?: string; managed: number; unmanaged?: HostErrorPageRoot[]; unmanaged_hosts: number; detail?: string }
export interface HostFirewallPlan { changes: { action: string; command: string }[]; unmanaged?: string[]; kept?: string[]; warnings?: string[]; fingerprint: string; confirm_seconds: number }
export type RedisUse = 'cache' | 'queue' | 'persistent';
export interface TuningOverrides { paused?: boolean; postgres?: Record<string, string>; connection_caps?: Record<string, number>; redis_max_memory_mib?: Record<string, number>; redis_use?: Record<string, RedisUse>; clickhouse_memory_ratio?: number }
export interface TuningSetting { name: string; current?: string; computed: string; override?: string; target: string; restart?: boolean; pending?: boolean; formula?: string }
export interface TuningCap { app_id: string; role: string; current: number; computed: number; override?: number; target: number; in_use: number; peak: number; pool?: number; pool_source?: string; superuser?: boolean; deferred?: boolean; warning?: string }
export interface TuningRedisApp { app_id: string; dbs?: number[]; use: RedisUse; use_source: 'override' | 'probe' | 'default'; can_move?: boolean; move_blocked?: string }
export interface TuningRedis { id: string; kind: 'shared' | 'own' | 'container'; apps: TuningRedisApp[]; used_bytes: number; max_bytes: number; policy?: string; target_mib: number; target_policy: string; override?: number; alert?: boolean; note?: string; error?: string }
export interface TuningClickHouse { id: string; kind: 'native' | 'container'; users: number; current_ratio?: number; target_ratio: number; server_mib: number; query_memory_mib: number; override?: number }
export interface TuningContainer { app_id: string; kind: string; container: string; settings: TuningSetting[]; note?: string }
export interface TuningChange { resource: string; setting: string; old: string; new: string; apply: 'reload' | 'restart' | 'live' | 'next-restart' | 'deferred'; note?: string }
export interface RedisMigration { app_id: string; dbs: number[]; use: RedisUse; status: 'running' | 'succeeded' | 'failed'; step?: string; copied?: number; started: string; finished?: string; error?: string }
export interface ResourceTuningView {
  supported: boolean; reason?: string; host: { cpus: number; memory_mib: number; disk_gib: number; rotational?: boolean }; apps: number; computed?: string; paused?: boolean;
  overrides: TuningOverrides;
  postgres?: { cluster: string; port: number; settings: TuningSetting[]; app_pool: number; headroom: number; in_use: number; conf_file?: string; note?: string };
  caps?: TuningCap[]; redis?: TuningRedis[]; clickhouse?: TuningClickHouse[]; containers?: TuningContainer[]; changes?: TuningChange[];
  pending_restart?: string[]; restart_note?: string; next_window?: string; warnings?: string[]; running?: boolean; migrations?: RedisMigration[]; jobs?: HostJob[];
}
