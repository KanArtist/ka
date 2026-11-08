import { Routes } from '@angular/router';
import { Activity } from './views/activity/activity';
import { TicketPage } from './views/peek/ticket-page';
import { Board } from './views/board/board';
import { Table } from './views/table/table';
import { Docs } from './views/docs/docs';
import { Agents } from './views/agents/agents';
import { Inbox } from './views/inbox/inbox';

export const routes: Routes = [
  { path: '', pathMatch: 'full', redirectTo: 'activity' },
  { path: 'activity', component: Activity },
  { path: 'board', component: Board },
  { path: 'table', component: Table },
  { path: 'docs', component: Docs },
  { path: 'agents', component: Agents },
  { path: 'inbox', component: Inbox },
  { path: 'ticket/:id', component: TicketPage },
  { path: '**', redirectTo: 'activity' },
];
