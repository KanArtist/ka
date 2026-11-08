import { Routes } from '@angular/router';
import { Stub } from './views/stub';

export const routes: Routes = [
  { path: '', pathMatch: 'full', redirectTo: 'activity' },
  { path: 'activity', component: Stub, data: { title: 'Activity' } },
  { path: 'board', component: Stub, data: { title: 'Board' } },
  { path: 'agents', component: Stub, data: { title: 'Agents' } },
  { path: 'inbox', component: Stub, data: { title: 'Inbox' } },
  { path: 'ticket/:id', component: Stub, data: { title: 'Ticket' } },
  { path: '**', redirectTo: 'activity' },
];
