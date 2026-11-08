import { Routes } from '@angular/router';
import { Stub } from './views/stub';
import { Activity } from './views/activity/activity';
import { TicketPage } from './views/peek/ticket-page';

export const routes: Routes = [
  { path: '', pathMatch: 'full', redirectTo: 'activity' },
  { path: 'activity', component: Activity },
  { path: 'board', component: Stub, data: { title: 'Board' } },
  { path: 'agents', component: Stub, data: { title: 'Agents' } },
  { path: 'inbox', component: Stub, data: { title: 'Inbox' } },
  { path: 'ticket/:id', component: TicketPage },
  { path: '**', redirectTo: 'activity' },
];
