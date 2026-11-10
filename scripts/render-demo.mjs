import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

const directory = process.argv[2];
if (!directory) throw new Error('Usage: node scripts/render-demo.mjs <demo output directory>');
const transcript = await readFile(path.join(directory, 'transcript.txt'), 'utf8');
const lines = transcript.trimEnd().split(/\r?\n/);
const escape = text => text.replace(/[&<>"']/g, value => ({
  '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&apos;',
})[value]);
const rows = lines.map((line, index) => {
  const color = line.startsWith('$') ? '#93c5fd' : /FAIL|exit code: 1/.test(line) ? '#fda4af' : /PASS|Verified:/.test(line) ? '#86efac' : '#e2e8f0';
  return '<text x="24" y="' + (86 + index * 22) + '" fill="' + color + '">' + escape(line) + '</text>';
}).join('\n');
const width = Math.max(1000, Math.max(...lines.map(line => line.length)) * 10 + 48);
const height = 116 + lines.length * 22;
await writeFile(path.join(directory, 'terminal.svg'), [
  '<svg xmlns="http://www.w3.org/2000/svg" width="' + width + '" height="' + height + '" viewBox="0 0 ' + width + ' ' + height + '">',
  '<title>DeployGuard: recorded output from the published binary on synthetic fixtures</title>',
  '<rect width="100%" height="100%" rx="12" fill="#0f172a"/>',
  '<g font-family="ui-monospace, SFMono-Regular, Consolas, monospace" font-size="16">',
  '<text x="24" y="32" fill="#f8fafc">DeployGuard · recorded CLI output</text>',
  '<text x="24" y="56" fill="#94a3b8">Synthetic loopback demo · FAIL is the expected regression result</text>',
  rows, '</g></svg>', '',
].join('\n'));
