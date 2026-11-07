import type { HTMLAttributes, ReactNode } from 'react';
import { SectionHeader, cx } from '../ui';

/*
 * Layout primitives. Spacing between siblings always comes from the parent's
 * gap (tokens in ui/src/tokens.css, rules in ui/src/layout.css); the children
 * themselves carry no outer margins.
 *
 *   <Page>     vertical flow of a whole view, --page-gap between sections
 *   <Section>  heading block (title, description, actions) then its body,
 *              --section-gap between them; body blocks --page-gap apart
 *   <Stack>    vertical (or row) flow with a named gap token
 *   <Cluster>  wrapping row of buttons, badges or chips, --control-gap apart
 */

export type StackGap = '1' | 'sm' | 'md' | 'form' | 'section' | 'page';

export function Page({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cx('page', className)} {...props} />;
}

export function Section({ title, description, actions, className, children, ...props }: Omit<HTMLAttributes<HTMLElement>, 'title'> & { title: string; description?: ReactNode; actions?: ReactNode }) {
  // The body is its own flow: blocks inside a section (usually cards) sit --page-gap apart.
  return <section className={cx('section', className)} aria-label={props['aria-label'] || title} {...props}><SectionHeader title={title} description={description} actions={actions} /><div className="section-body">{children}</div></section>;
}

export function Stack({ gap = 'md', direction = 'column', className, ...props }: HTMLAttributes<HTMLDivElement> & { gap?: StackGap; direction?: 'column' | 'row' }) {
  return <div className={cx('stack', `stack-gap-${gap}`, direction === 'row' && 'stack-row', className)} {...props} />;
}

export function Cluster({ justify = 'start', className, ...props }: HTMLAttributes<HTMLDivElement> & { justify?: 'start' | 'end' | 'between' }) {
  return <div className={cx('cluster', justify !== 'start' && `cluster-${justify}`, className)} {...props} />;
}
