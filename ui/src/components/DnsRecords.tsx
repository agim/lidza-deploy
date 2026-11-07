import { CopyButton } from './ui';

export type DnsRecord = { hostname: string; type: string; value: string; note?: string };

/** dnsAddressRecords are the A/AAAA records pointing hostname at addresses. */
export function dnsAddressRecords(hostname: string, addresses: string[], note?: string): DnsRecord[] {
  return addresses.map(value => ({ hostname, type: value.includes(':') ? 'AAAA' : 'A', value, ...(note ? { note } : {}) }));
}

/** DnsRecordsTable lists records to set at a DNS provider, each value with a copy button (moves, redirect domains). */
export function DnsRecordsTable({ records, label }: { records: DnsRecord[]; label?: string }) {
  return <table className="move-records" aria-label={label}><thead><tr><th>Hostname</th><th>Type</th><th>Value</th><th><span className="sr-only">Copy</span></th></tr></thead>
    <tbody>{records.map(record => <tr key={`${record.hostname}-${record.type}-${record.value}`}><td><code>{record.hostname}</code></td><td>{record.type}</td><td><code>{record.value}</code>{record.note && <small className="move-record-note">{record.note}</small>}</td><td><CopyButton value={record.value} label={`Copy ${record.type} value for ${record.hostname}`} /></td></tr>)}</tbody></table>;
}
