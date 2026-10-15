import { useState } from 'react';
import { Info, Plus } from 'lucide-react';
import { Button, Card, SectionHeader } from './ui';
import { Input } from './primitives/input';
import { Label } from './primitives/label';
import { Switch } from './primitives/switch';
import { Separator } from './primitives/separator';
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription, DialogTrigger } from './primitives/dialog';
import { Sheet, SheetTrigger, SheetContent, SheetHeader, SheetTitle, SheetDescription } from './primitives/sheet';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from './primitives/select';
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from './primitives/tooltip';
import { useToast } from './ToastProvider';

export function ComponentGallery() {
  const [enabled, setEnabled] = useState(true);
  const [region, setRegion] = useState('production');
  const notify = useToast();
  return <section className="component-gallery" aria-label="Console components"><SectionHeader title="Built to work together" description="The same shadcn/ui building blocks are used throughout your console. These examples only change this preview." />
    <Card className="gallery-section"><h3>Actions & feedback</h3><div className="component-example"><Button variant="primary" onClick={async () => { notify('Example action completed.'); }}><Plus size={15} />Primary action</Button><Button>Secondary action</Button><Button variant="ghost">Quiet action</Button><Button disabled>Unavailable</Button><TooltipProvider><Tooltip><TooltipTrigger asChild><Button size="icon" aria-label="About these controls"><Info size={16} /></Button></TooltipTrigger><TooltipContent>Shared controls, consistent across every page.</TooltipContent></Tooltip></TooltipProvider></div><Separator /><div className="component-example"><Dialog><DialogTrigger asChild><Button>Open example dialog</Button></DialogTrigger><DialogContent><DialogHeader><DialogTitle>A focused conversation</DialogTitle><DialogDescription>Dialogs keep short decisions in context. This example makes no server changes.</DialogDescription></DialogHeader></DialogContent></Dialog><Sheet><SheetTrigger asChild><Button>Open example panel</Button></SheetTrigger><SheetContent><SheetHeader><SheetTitle>Room for the details</SheetTitle><SheetDescription>The same accessible panel powers mobile navigation. This example makes no server changes.</SheetDescription></SheetHeader></SheetContent></Sheet></div></Card>
    <Card className="gallery-section"><h3>Fields & choices</h3><div className="form-grid two"><div className="field"><Label htmlFor="example-name">Application name</Label><Input id="example-name" placeholder="customer-portal" /></div><div className="field"><Label htmlFor="example-environment">Environment</Label><Select value={region} onValueChange={setRegion}><SelectTrigger id="example-environment"><SelectValue /></SelectTrigger><SelectContent><SelectItem value="production">Production</SelectItem><SelectItem value="staging">Staging</SelectItem></SelectContent></Select></div></div><div className="component-example"><Switch id="example-auto" checked={enabled} onCheckedChange={setEnabled} /><Label htmlFor="example-auto">Deploy automatically {enabled ? 'enabled' : 'disabled'}</Label></div></Card>
  </section>;
}
