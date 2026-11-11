import { Component, type ErrorInfo, type ReactNode } from 'react';
import { ErrorBanner } from './ui';

// SectionBoundary keeps a rendering failure inside the part of the page that
// failed: the rest of the page stays usable and the operator sees which part
// broke, instead of a blank screen. `resetKey` clears the error when the
// route or the data it renders changes.
type Props = { label: string; resetKey?: unknown; children: ReactNode };
type State = { error: Error | null; key: unknown };

export class SectionBoundary extends Component<Props, State> {
  state: State = { error: null, key: this.props.resetKey };

  static getDerivedStateFromProps(props: Props, state: State): Partial<State> | null {
    return props.resetKey !== state.key ? { error: null, key: props.resetKey } : null;
  }

  static getDerivedStateFromError(error: Error): Partial<State> {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error(`${this.props.label} failed to render`, error, info.componentStack);
  }

  render() {
    if (!this.state.error) return this.props.children;
    return <ErrorBanner>{this.props.label} could not be shown: {this.state.error.message}. The rest of the page still works; reload to try again.</ErrorBanner>;
  }
}
