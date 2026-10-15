import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Button } from './primitives/button';
import { Input } from './primitives/input';
import { Label } from './primitives/label';
import { Badge } from './primitives/badge';
import { Card, CardHeader, CardTitle, CardDescription, CardAction, CardContent, CardFooter } from './primitives/card';
import { Separator } from './primitives/separator';
import { Switch } from './primitives/switch';
import { Tabs, TabsList, TabsTrigger, TabsContent } from './primitives/tabs';
import { Dialog, DialogTrigger, DialogContent, DialogHeader, DialogTitle, DialogDescription, DialogFooter, DialogClose } from './primitives/dialog';
import { Sheet, SheetTrigger, SheetContent, SheetHeader, SheetTitle, SheetDescription, SheetFooter, SheetClose } from './primitives/sheet';
import { Select, SelectTrigger, SelectValue, SelectContent, SelectItem, SelectGroup, SelectLabel, SelectSeparator } from './primitives/select';
import { DropdownMenu, DropdownMenuTrigger, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuCheckboxItem, DropdownMenuRadioGroup, DropdownMenuRadioItem, DropdownMenuGroup, DropdownMenuShortcut } from './primitives/dropdown-menu';
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from './primitives/tooltip';
import { Table, TableHeader, TableRow, TableHead, TableBody, TableCell, TableCaption, TableFooter } from './primitives/table';
import { ToastProvider } from './ToastProvider';
import { useToast } from './toast-context';
import { toast } from 'sonner';

afterEach(() => { cleanup(); toast.dismiss(); });

describe('template primitives', () => {
  it('keeps button link semantics and disables unavailable actions', () => {
    const action = vi.fn();
    render(<><Button disabled onClick={action}>Remove</Button><Button asChild><a href="#servers">Hosts</a></Button></>);
    fireEvent.click(screen.getByRole('button', { name: 'Remove' }));
    expect(action).not.toHaveBeenCalled();
    expect(screen.getByRole('link', { name: 'Hosts' })).toHaveAttribute('href', '#servers');
  });
  it('preserves associated fields, credential types, and form values', () => {
    render(<form aria-label="Environment"><Label htmlFor="token">Token</Label><Input id="token" name="token" type="password" required defaultValue="secret" /></form>);
    expect(screen.getByLabelText('Token')).toHaveAttribute('type', 'password');
    expect(new FormData(screen.getByRole('form') as HTMLFormElement).get('token')).toBe('secret');
    fireEvent.change(screen.getByLabelText('Token'), { target: { value: '' } });
    expect(screen.getByLabelText('Token')).toBeInvalid();
  });
  it('renders reusable card regions, statuses and semantic separators', () => {
    render(<Card><CardHeader><CardTitle>Host</CardTitle><CardDescription>Production</CardDescription><CardAction><Badge>Healthy</Badge></CardAction></CardHeader><CardContent>4 cores</CardContent><Separator decorative={false} orientation="vertical" /><CardFooter>Updated now</CardFooter></Card>);
    for (const text of ['Host', 'Production', 'Healthy', '4 cores', 'Updated now']) expect(screen.getByText(text)).toBeInTheDocument();
    expect(screen.getByRole('separator')).toHaveAttribute('aria-orientation', 'vertical');
  });
  it('submits checked switches and omits unchecked or disabled switches', () => {
    render(<form aria-label="Automation"><Label htmlFor="auto">Auto deploy</Label><Switch id="auto" name="auto" defaultChecked /><Switch name="disabled" aria-label="Disabled automation" disabled /></form>);
    const form = screen.getByRole('form') as HTMLFormElement;
    expect(new FormData(form).get('auto')).toBe('on');
    fireEvent.click(screen.getByRole('switch', { name: 'Auto deploy' }));
    expect(new FormData(form).has('auto')).toBe(false);
    expect(new FormData(form).has('disabled')).toBe(false);
  });
  it('moves between tab panels without losing accessible selection', async () => {
    render(<Tabs defaultValue="general"><TabsList><TabsTrigger value="general">General</TabsTrigger><TabsTrigger value="env">Environment</TabsTrigger></TabsList><TabsContent value="general">General settings</TabsContent><TabsContent value="env">Secret settings</TabsContent></Tabs>);
    fireEvent.mouseDown(screen.getByRole('tab', { name: 'Environment' }), { button: 0, ctrlKey: false });
    expect(await screen.findByRole('tabpanel')).toHaveTextContent('Secret settings');
    expect(screen.getByRole('tab', { name: 'Environment' })).toHaveAttribute('aria-selected', 'true');
  });
  it('opens a titled dialog, closes with Escape and restores trigger focus', async () => {
    render(<Dialog><DialogTrigger asChild><Button>Open dialog</Button></DialogTrigger><DialogContent><DialogHeader><DialogTitle>Confirm scope</DialogTitle><DialogDescription>Only this server.</DialogDescription></DialogHeader><DialogFooter><DialogClose asChild><Button>Cancel</Button></DialogClose></DialogFooter></DialogContent></Dialog>);
    fireEvent.click(screen.getByRole('button', { name: 'Open dialog' }));
    expect(screen.getByRole('dialog', { name: 'Confirm scope' })).toHaveAccessibleDescription('Only this server.');
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await waitFor(() => expect(screen.getByRole('button', { name: 'Open dialog' })).toHaveFocus());
  });
  it('opens and closes accessible side panels', async () => {
    render(<Sheet><SheetTrigger asChild><Button>Open panel</Button></SheetTrigger><SheetContent><SheetHeader><SheetTitle>Navigation</SheetTitle><SheetDescription>Choose a destination.</SheetDescription></SheetHeader><SheetFooter><SheetClose asChild><Button>Done</Button></SheetClose></SheetFooter></SheetContent></Sheet>);
    fireEvent.click(screen.getByRole('button', { name: 'Open panel' }));
    expect(screen.getByRole('dialog', { name: 'Navigation' })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Done' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });
  it('selects values through the accessible select and submits its form value', async () => {
    render(<form aria-label="Host choice"><Select name="host" defaultValue="local"><SelectTrigger aria-label="Host"><SelectValue /></SelectTrigger><SelectContent><SelectGroup><SelectLabel>Connected</SelectLabel><SelectItem value="local">Local</SelectItem><SelectSeparator /><SelectItem value="remote">Remote</SelectItem></SelectGroup></SelectContent></Select></form>);
    fireEvent.keyDown(screen.getByRole('combobox'), { key: 'ArrowDown' });
    fireEvent.click(await screen.findByRole('option', { name: 'Remote' }));
    expect(new FormData(screen.getByRole('form') as HTMLFormElement).get('host')).toBe('remote');
  });
  it('supports action, checkbox and radio menu items with disabled guards', async () => {
    const action = vi.fn(), checked = vi.fn(), radio = vi.fn();
    render(<DropdownMenu defaultOpen><DropdownMenuTrigger>Actions</DropdownMenuTrigger><DropdownMenuContent><DropdownMenuLabel>Operations</DropdownMenuLabel><DropdownMenuGroup><DropdownMenuItem disabled onSelect={action}>Remove</DropdownMenuItem><DropdownMenuCheckboxItem checked onCheckedChange={checked} onSelect={event => event.preventDefault()}>Auto deploy</DropdownMenuCheckboxItem></DropdownMenuGroup><DropdownMenuSeparator /><DropdownMenuRadioGroup value="local" onValueChange={radio}><DropdownMenuRadioItem value="local">Local</DropdownMenuRadioItem><DropdownMenuRadioItem value="remote" onSelect={event => event.preventDefault()}>Remote<DropdownMenuShortcut>R</DropdownMenuShortcut></DropdownMenuRadioItem></DropdownMenuRadioGroup></DropdownMenuContent></DropdownMenu>);
    fireEvent.click(screen.getByRole('menuitem', { name: 'Remove' }));
    expect(action).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('menuitemcheckbox', { name: 'Auto deploy' }));
    expect(checked).toHaveBeenCalledWith(false);
    fireEvent.click(screen.getByRole('menuitemradio', { name: 'Remote R' }));
    expect(radio).toHaveBeenCalledWith('remote');
  });
  it('shows keyboard-focus tooltips', async () => {
    render(<TooltipProvider delayDuration={0}><Tooltip><TooltipTrigger asChild><Button>Details</Button></TooltipTrigger><TooltipContent>Current host capacity</TooltipContent></Tooltip></TooltipProvider>);
    await act(async () => { screen.getByRole('button').focus(); });
    expect(await screen.findByRole('tooltip')).toHaveTextContent('Current host capacity');
  });
  it('preserves table headers, caption, data and totals', () => {
    render(<Table><TableCaption>Resources</TableCaption><TableHeader><TableRow><TableHead>Host</TableHead><TableHead>RAM</TableHead></TableRow></TableHeader><TableBody><TableRow><TableCell>Production</TableCell><TableCell>8 GiB</TableCell></TableRow></TableBody><TableFooter><TableRow><TableCell colSpan={2}>One server</TableCell></TableRow></TableFooter></Table>);
    expect(screen.getByRole('table')).toHaveAccessibleName('Resources');
    expect(screen.getAllByRole('columnheader')).toHaveLength(2);
    expect(screen.getByRole('cell', { name: '8 GiB' })).toBeInTheDocument();
    expect(screen.getByRole('cell', { name: 'One server' })).toHaveAttribute('colspan', '2');
  });
  it('announces operation results and persistent errors through the shared toaster', async () => {
    function Notify() { const notify = useToast(); return <><button onClick={() => notify('Saved')}>Save</button><button onClick={() => notify('Agent offline', true)}>Fail</button></>; }
    render(<ToastProvider><Notify /></ToastProvider>);
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('Saved')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Fail' }));
    expect(await screen.findByText('Agent offline')).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: 'Close toast' }).length).toBeGreaterThan(0);
  });
});
