// Keep the embedded bundle lean: drop files the daemon never serves.
import { rmSync, existsSync } from 'node:fs';
const out = new URL('../../internal/api/ui/', import.meta.url);
for (const f of ['3rdpartylicenses.txt.map', 'prerendered-routes.json']) {
  const p = new URL(f, out);
  if (existsSync(p)) rmSync(p);
}
