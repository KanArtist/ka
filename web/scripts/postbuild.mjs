// Normalize the build output so the committed bundle is byte-stable across
// machines (CI compares it with a fresh build), and stamp it with the API
// it was generated against (npm run gen:api writes api-hash), so a daemon
// serves a checkout's ui/ only when it fits.
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
const lic = new URL('../../ui/3rdpartylicenses.txt', import.meta.url);
if (existsSync(lic)) writeFileSync(lic, readFileSync(lic, 'utf8').replace(/\r\n/g, '\n'));
const api = readFileSync(new URL('../api-hash', import.meta.url), 'utf8').trim();
writeFileSync(new URL('../../ui/ka-ui.json', import.meta.url), JSON.stringify({ api }, null, 2) + '\n');
