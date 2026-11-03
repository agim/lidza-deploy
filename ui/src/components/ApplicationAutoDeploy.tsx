import {GitBranch} from 'lucide-react';
import {del,post} from '../lib/api';
import {MenuItem} from './ui';
import {useToast} from './ToastProvider';

export function ApplicationAutoDeploy({appID,enabled,disabled,requireAppPairing,onChange}:{appID:string;enabled?:boolean;disabled?:boolean;requireAppPairing?:boolean;onChange:()=>Promise<void>}) {
  const notify=useToast();
  return <MenuItem disabled={disabled&&!enabled} icon={<GitBranch size={16}/>} onSelect={async()=>{
    try {
      if(new URLSearchParams(location.search).get('demo')!=='1') {
        const path=`control/apps/${encodeURIComponent(appID)}`;
        if(enabled)await del(`${path}/webhook`);
        else {
          if(requireAppPairing)await post(`${path}/github-app`,{});
          await post(`${path}/webhook`,{});
        }
      }
      await onChange();notify(enabled?'Auto-deploy disabled.':'Auto-deploy enabled for the selected branch.');
    } catch(error) { notify(error instanceof Error?error.message:String(error),true); }
  }}>{enabled?'Disable auto-deploy':'Enable auto-deploy'}</MenuItem>;
}
