export interface SpacingProblem { line: number; message: string; file?: string }
export function cssViolations(text: string): SpacingProblem[];
export function tsxViolations(text: string): SpacingProblem[];
export function checkConsole(): { files: number; problems: Array<SpacingProblem & { file: string }> };
