import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import App from './App';
import { ToastProvider } from './components/ToastProvider';
import { WorkspaceProvider } from './state/WorkspaceContext';
import './styles.css';

createRoot(document.getElementById('root')!).render(<StrictMode><ToastProvider><WorkspaceProvider><App /></WorkspaceProvider></ToastProvider></StrictMode>);
