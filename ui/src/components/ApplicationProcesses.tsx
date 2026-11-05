import {useEffect,useRef,useState} from 'react';
import {get} from '../lib/api';
import {Button,Card,ErrorBanner,Loading,LogOutputHeader,SectionHeader,StatusBadge} from './ui';

type Process={id:string;name:string;kind:string;state:string;startup?:string;log_path?:string};
type ProcessLog={process:Process;output:string;loading:boolean;error?:string};
export function ApplicationProcesses({appID}:{appID:string}) {
  const [data,setData]=useState<{processes:Process[];note?:string}>(),[error,setError]=useState(''),[log,setLog]=useState<ProcessLog>();
  const logRequest=useRef(0),activeApp=useRef(appID);
  activeApp.current=appID;
  const load=async()=>{try{const next=await get<{processes:Process[];note?:string}>(`control/apps/${encodeURIComponent(appID)}/processes`);if(activeApp.current===appID){setData(next);setError('');}}catch(reason){if(activeApp.current===appID)setError(reason instanceof Error?reason.message:String(reason));}};
  useEffect(()=>{setData(undefined);setLog(undefined);void load();const timer=setInterval(()=>{if(!document.hidden)void load();},15000);return()=>{clearInterval(timer);logRequest.current++;};},[appID]);
  const readLog=async(process:Process,keepOutput=false)=>{
    if(!process.log_path)return;
    const request=++logRequest.current;
    setLog(current=>({process,output:keepOutput&&current?.process.id===process.id?current.output:'',loading:true}));
    try {
      const next=await get<{log:string}>(process.log_path);
      if(request===logRequest.current)setLog({process,output:next.log,loading:false});
    } catch(reason) {
      if(request===logRequest.current)setLog(current=>({process,output:current?.output||'',loading:false,error:reason instanceof Error?reason.message:String(reason)}));
    }
  };
  const closeLog=()=>{logRequest.current++;setLog(undefined);};
  return <Card className="form-card application-processes"><SectionHeader title="Processes" description="Running state and startup policy are shown separately." actions={<Button type="button" onClick={load}>Refresh</Button>}/>{error&&<ErrorBanner>{error}</ErrorBanner>}{!data&&!error&&<Loading label="Loading processes…"/>}{data?.note&&<p className="muted-copy">{data.note}</p>}{data?.processes.map(process=><div className="resource-setting application-process" key={process.id}><div><strong>{process.name}</strong><p>{process.kind}</p><StatusBadge status={process.state||'unknown'}/>{process.startup&&<p>Startup: {process.startup}</p>}</div>{process.log_path&&<Button type="button" aria-label={`View logs for ${process.name}`} aria-pressed={log?.process.id===process.id} onClick={()=>void readLog(process)}>View logs</Button>}</div>)}{data&&!data.processes.length&&<p>No application processes reported.</p>}{log&&<section className="process-log-panel" aria-label={`${log.process.name} logs`}><LogOutputHeader title={`${log.process.name} logs`} value={log.output} copyLabel="Copy log"/><div className="process-log-actions"><Button type="button" disabled={log.loading} onClick={()=>void readLog(log.process,true)}>Refresh log</Button><Button type="button" onClick={closeLog}>Close log</Button></div>{log.error&&<ErrorBanner>{log.error}</ErrorBanner>}{log.loading&&<p role="status">Loading log output…</p>}<pre className="console-output process-log-output" role="region" aria-label={`${log.process.name} log output`} tabIndex={0}>{log.output||(!log.loading&&!log.error?'No log output available.':'')}</pre></section>}</Card>;
}
