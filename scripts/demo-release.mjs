import { createHash } from 'node:crypto';
import { execFile } from 'node:child_process';
import { mkdir, mkdtemp, readFile, writeFile } from 'node:fs/promises';
import http from 'node:http';
import path from 'node:path';
import { parseArgs, promisify } from 'node:util';

const execute = promisify(execFile);
const { values } = parseArgs({
  options: { binary: { type: 'string' }, out: { type: 'string' } },
});
if (!values.binary) throw new Error('Usage: node scripts/demo-release.mjs --binary <deployguard executable> [--out <empty directory>]');
const binary = path.resolve(values.binary);
const binaryBytes = await readFile(binary);
let output;
if (values.out) {
  output = path.resolve(values.out);
  await mkdir(output, { recursive: false });
} else {
  await mkdir('reports', { recursive: true });
  output = await mkdtemp(path.resolve('reports', 'published-demo-'));
}
let candidateMode = 'clean';
const baselineData = { id: 42, email: 'demo@example.invalid' };
const servers = [];
let transcript = '';
function record(text) {
  transcript += text;
  process.stdout.write(text);
}
async function serve(candidate) {
  const server = http.createServer((request, response) => {
    response.setHeader('Content-Type', 'application/json');
    if (request.url !== '/api/users/42' || request.method !== 'GET') {
      response.writeHead(404).end('{}');
      return;
    }
    const data = candidate
      ? candidateMode === 'clean'
        ? { email: baselineData.email, id: baselineData.id }
        : { id: baselineData.id }
      : baselineData;
    response.end(JSON.stringify(data));
  });
  servers.push(server);
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  return 'http://127.0.0.1:' + server.address().port;
}
try {
  const version = (await execute(binary, ['version'], { timeout: 10000 })).stdout.trim();
  record('$ deployguard version\n' + version + '\n\n');
  const baseline = await serve(false);
  const candidate = await serve(true);
  const config = path.join(output, 'workload.yaml');
  await writeFile(config, [
    'version: 1', 'settings:', '  timeout: 2s', '  max_total_requests: 100',
    'scenarios:', '  - name: get-user', '    method: GET', '    path: /api/users/42', '',
  ].join('\n'));
  const cases = [];
  for (const [name, expectedExit, expectedOutcome] of [
    ['clean', 0, 'PASS'], ['regressed', 1, 'FAIL'],
  ]) {
    candidateMode = name;
    const reportFile = path.join(output, name + '.json');
    const args = ['compare', '--config', config, '--baseline', baseline, '--candidate', candidate, '--report', reportFile];
    record('$ deployguard compare  # ' + name + ' synthetic candidate\n');
    let result;
    let exit = 0;
    try {
      result = await execute(binary, args, { timeout: 30000, maxBuffer: 1024 * 1024 });
    } catch (error) {
      if (!Number.isInteger(error.code)) throw error;
      exit = error.code;
      result = error;
    }
    record((result.stdout || '') + (result.stderr || '') + 'Observed exit code: ' + exit + '\n\n');
    const report = JSON.parse(await readFile(reportFile, 'utf8'));
    if (exit !== expectedExit || report.outcome !== expectedOutcome || report.exit_code !== expectedExit) {
      throw new Error(name + ' did not produce its expected outcome');
    }
    if (name === 'regressed' && !JSON.stringify(report).includes('field_removed')) {
      throw new Error('The regressed report did not identify the removed field');
    }
    cases.push({ name, exitCode: exit, outcome: report.outcome, report: name + '.json' });
  }
  const evidence = {
    recordedAt: new Date().toISOString(),
    platform: process.platform,
    architecture: process.arch,
    version,
    binarySha256: createHash('sha256').update(binaryBytes).digest('hex'),
    workflowRun: process.env.GITHUB_RUN_ID
      ? 'https://github.com/' + process.env.GITHUB_REPOSITORY + '/actions/runs/' + process.env.GITHUB_RUN_ID
      : null,
    scriptCommit: process.env.GITHUB_SHA || null,
    scope: 'Two synthetic loopback cases against the executable passed with --binary; not a production benchmark.',
    cases,
  };
  await writeFile(path.join(output, 'evidence.json'), JSON.stringify(evidence, null, 2) + '\n');
  record('Verified: harmless JSON reordering passes; removal of /email blocks the release.\n');
} finally {
  await Promise.all(servers.map(server => new Promise(resolve => server.close(resolve))));
  await writeFile(path.join(output, 'transcript.txt'), transcript);
}
