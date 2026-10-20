import type { SelectHTMLAttributes } from 'react';
import { Field } from './ui';

export function RuntimeProfileField({ allowSingle = true, ...props }: SelectHTMLAttributes<HTMLSelectElement> & { allowSingle?: boolean }) {
  return <Field label="Runtime profile" hint="Stacks require deploy/lidza.stack.json. Budgets include a new dedicated PostgreSQL database. Existing databases retain their sizing; control-panel overhead is additional. Profile changes briefly stop the stack.">
    <select aria-label="Runtime profile" {...props}>
      {allowSingle && <option value="">Single container · existing secure runtime</option>}
      <option value="minimum">Minimum · 4 GiB / 2 CPU</option>
      <option value="standard">Standard · 8 GiB / 4 CPU</option>
      <option value="performance">Performance · 16 GiB / 8 CPU</option>
    </select>
  </Field>;
}
