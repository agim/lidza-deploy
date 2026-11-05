import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import App from './App';
import { PreferencesProvider } from './state/PreferencesContext';
import { ToastProvider } from './components/ToastProvider';
import { WorkspaceProvider } from './state/WorkspaceContext';
import { ThemeProvider } from 'next-themes';
import 'sonner/dist/styles.css';
import './styles.css';
import './template.css';
import './console.css';

createRoot(document.getElementById('root')!).render(<StrictMode><ThemeProvider attribute="class" forcedTheme="dark"><ToastProvider><PreferencesProvider><WorkspaceProvider><App /></WorkspaceProvider></PreferencesProvider></ToastProvider></ThemeProvider></StrictMode>);
