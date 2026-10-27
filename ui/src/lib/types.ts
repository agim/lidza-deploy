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
