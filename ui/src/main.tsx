import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import App from './App';
import { PreferencesProvider } from './state/PreferencesContext';
import { ToastProvider } from './components/ToastProvider';
import { WorkspaceProvider } from './state/WorkspaceContext';
import { startRouter } from './lib/router';
import 'sonner/dist/styles.css';
import './styles.css';
import './template.css';
import './console.css';
import './layout.css';

startRouter();
createRoot(document.getElementById('root')!).render(<StrictMode><ToastProvider><PreferencesProvider><WorkspaceProvider><App /></WorkspaceProvider></PreferencesProvider></ToastProvider></StrictMode>);
