import { Routes } from '@angular/router';
import { Activity } from './views/activity/activity';
import { Board } from './views/board/board';

// Activity and Board load with the app; the other views load on first use.
export const routes: Routes = [
  { path: '', pathMatch: 'full', redirectTo: 'activity' },
  { path: 'activity', component: Activity },
  { path: 'board', component: Board },
  { path: 'table', loadComponent: () => import('./views/table/table').then((m) => m.Table) },
  { path: 'docs', loadComponent: () => import('./views/docs/docs').then((m) => m.Docs) },
  { path: 'health', loadComponent: () => import('./views/health/health').then((m) => m.Health) },
  { path: 'settings', loadComponent: () => import('./views/settings/settings').then((m) => m.Settings) },
  { path: 'agents', loadComponent: () => import('./views/agents/agents').then((m) => m.Agents) },
  { path: 'inbox', loadComponent: () => import('./views/inbox/inbox').then((m) => m.Inbox) },
  { path: 'ticket/:id', loadComponent: () => import('./views/peek/ticket-page').then((m) => m.TicketPage) },
  { path: '**', redirectTo: 'activity' },
];
