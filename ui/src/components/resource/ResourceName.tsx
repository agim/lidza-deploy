import { createContext, type ReactNode, useContext, useEffect, useState } from 'react';

// The display name of the resource a detail view shows, for the breadcrumb
// and the document title (the URL carries only its id).
const ResourceNameContext = createContext<{ name?: string; setName: (name?: string) => void }>({ setName: () => undefined });

export function ResourceNameProvider({ children }: { children: ReactNode }) {
  const [name, setName] = useState<string>();
  return <ResourceNameContext.Provider value={{ name, setName }}>{children}</ResourceNameContext.Provider>;
}

export function useResourceName() { return useContext(ResourceNameContext).name; }

/** useNameResource names the shown resource until the view unmounts. */
export function useNameResource(name?: string) {
  const { setName } = useContext(ResourceNameContext);
  useEffect(() => { setName(name); return () => setName(undefined); }, [name, setName]);
}
