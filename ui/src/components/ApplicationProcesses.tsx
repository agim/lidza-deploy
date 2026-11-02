import {useEffect,useState} from 'react';
import {get} from '../lib/api';
import {Button,Card,ErrorBanner,Loading,SectionHeader,StatusBadge} from './ui';

type Process={id:string;name:string;kind:string;state:string;startup?:string;log_path?:string};
export function ApplicationProcesses({appID}:{appID:string}) {
  const [data,setData]=useState<{processes:Process[];note?:string}>(),[error,setError]=useState(''),[log,setLog]=useState('');
  const load=async()=>{try{setData(await get(`control/apps/${encodeURIComponent(appID)}/processes`));setError('');}catch(reason){setError(reason instanceof Error?reason.message:String(reason));}};
  useEffect(()=>{void load();const timer=setInterval(()=>{if(!document.hidden)void load();},15000);return()=>clearInterval(timer);},[appID]);
  return <Card className="form-card"><SectionHeader title="Processes" description="Running state and startup policy are shown separately." actions={<Button type="button" onClick={load}>Refresh</Button>}/>{error&&<ErrorBanner>{error}</ErrorBanner>}{!data&&!error&&<Loading label="Loading processes…"/>}{data?.note&&<p className="muted-copy">{data.note}</p>}{data?.processes.map(process=><div className="resource-setting" key={process.id}><div><strong>{process.name}</strong><p>{process.kind}</p><StatusBadge status={process.state||'unknown'}/>{process.startup&&<p>Startup: {process.startup}</p>}</div>{process.log_path&&<Button type="button" onClick={async()=>{try{setLog((await get<{log:string}>(process.log_path!)).log);setError('');}catch(reason){setError(reason instanceof Error?reason.message:String(reason));}}}>View logs</Button>}</div>)}{data&&!data.processes.length&&<p>No application processes reported.</p>}{log&&<pre className="terminal-output">{log}</pre>}</Card>;
}
