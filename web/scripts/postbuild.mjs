// Normalize the build output so the committed bundle is byte-stable across
// machines (CI compares it with a fresh build).
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
const lic = new URL('../../internal/api/ui/3rdpartylicenses.txt', import.meta.url);
if (existsSync(lic)) writeFileSync(lic, readFileSync(lic, 'utf8').replace(/\r\n/g, '\n'));
