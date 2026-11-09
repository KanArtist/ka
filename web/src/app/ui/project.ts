import { Pipe, PipeTransform, inject } from '@angular/core';
import { Live } from '../live';

/** {{ key | project }}: the project's name (from its config), or the key when it has none. */
@Pipe({ name: 'project', pure: false })
export class ProjectPipe implements PipeTransform {
  private live = inject(Live);
  transform(key: string | undefined | null): string {
    if (!key) return '';
    return this.live.schema()?.projects?.find((p) => p.key === key)?.name || key;
  }
}
