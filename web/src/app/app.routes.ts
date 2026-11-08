import { Routes } from '@angular/router';
import { Stub } from './views/stub';
import { Activity } from './views/activity/activity';
import { TicketPage } from './views/peek/ticket-page';
import { Board } from './views/board/board';
import { Agents } from './views/agents/agents';

export const routes: Routes = [
  { path: '', pathMatch: 'full', redirectTo: 'activity' },
  { path: 'activity', component: Activity },
  { path: 'board', component: Board },
  { path: 'agents', component: Agents },
  { path: 'inbox', component: Stub, data: { title: 'Inbox' } },
  { path: 'ticket/:id', component: TicketPage },
  { path: '**', redirectTo: 'activity' },
];
