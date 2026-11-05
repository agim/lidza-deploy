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
  errors?: string[];
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

export interface Application {
 management_capabilities?: {deploy?:boolean;rollback?:boolean;configure?:boolean;environment?:boolean};
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
  maintenance?: { enabled?: boolean };
  previews?: { enabled?: boolean; database?: boolean; base_domain?: string };
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
 capabilities?: {routing?:boolean;resources?:boolean;runtime_mode?:boolean;configure:boolean;environment:boolean;deploy:boolean;reload:boolean;rollback:boolean;processes:boolean;logs:boolean;reason?:string};
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
  | 'team' | 'audit' | 'help' | 'new-application' | 'detail';

export interface Route {
  page: PageKey;
  detail?: string;
  id?: string;
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
  data: { postgres?: boolean; redis?: boolean; external_database_url?: string; backup_hours?: number; offsite?: boolean };
  env?: Record<string, string>;
  env_kinds?: Record<string, string>;
  auto_deploy?: boolean;
  server_id: string;
  github_installation?: number;
  deploy?: boolean;
}

// Host management (agent host_updates.go / host_firewall.go).
export interface HostRebootWindow { weekdays?: number[]; hour: number; duration_minutes: number }
export interface HostUpdatesPolicy { auto_security: boolean; auto_reboot: boolean; reboot_window: HostRebootWindow; manage_package_sources: boolean }
export interface HostPackageUpgrade { name: string; current?: string; candidate: string; security: boolean }
export interface HostComponent { name: string; package?: string; installed?: string; candidate?: string; upgrade_available: boolean; source: 'package' | 'binary' | 'absent'; repository?: string; automatic: 'security' | 'all' | 'none' }
export interface HostJob { id: string; kind: string; trigger: string; status: 'running' | 'succeeded' | 'failed'; started: string; finished?: string; summary?: string; output?: string }
export interface HostUpdatesStatus {
  supported: boolean; reason?: string; checked_at?: string;
  pending: number; security: number; packages?: HostPackageUpgrade[];
  reboot_required: boolean; reboot_packages?: string[]; reboot_reason?: string;
  unattended_installed: boolean; unattended_enabled: boolean; unattended_last_run?: string;
  managed_origins?: string[]; components?: HostComponent[];
  policy: HostUpdatesPolicy; configured: boolean;
  next_reboot_window?: string; last_reboot?: string; reboot_deferred?: string;
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
export interface HostFirewallPlan { changes: { action: string; command: string }[]; unmanaged?: string[]; kept?: string[]; warnings?: string[]; fingerprint: string; confirm_seconds: number }
