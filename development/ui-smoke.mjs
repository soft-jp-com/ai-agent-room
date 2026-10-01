import { spawn } from 'node:child_process';
import { mkdir, mkdtemp, rm, stat } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';

const [edgePath, startURL, artifactDir] = process.argv.slice(2);
if (!edgePath || !startURL || !artifactDir) {
  throw new Error('usage: node ui-smoke.mjs <edge> <start-url> <artifact-dir>');
}

function runEdgeScreenshot(args) {
  return new Promise((resolve, reject) => {
    const child = spawn(edgePath, args, { stdio: ['ignore', 'pipe', 'pipe'], windowsHide: true });
    let stdout = '';
    let stderr = '';
    const timer = setTimeout(() => {
      child.kill();
      reject(new Error(`Headless Edge timed out (DOM bytes ${stdout.length}): ${stderr.replaceAll(token, '[redacted]').slice(-1500)}`));
    }, 25000);
    child.stdout.setEncoding('utf8');
    child.stderr.setEncoding('utf8');
    child.stdout.on('data', (data) => { stdout += data; });
    child.stderr.on('data', (data) => { stderr += data; });
    child.once('error', (error) => {
      clearTimeout(timer);
      reject(error);
    });
    child.once('close', (code) => {
      clearTimeout(timer);
      if (code === 0) resolve({ stdout, stderr });
      else reject(new Error(`Headless Edge exited with code ${code}: ${stderr.replaceAll(token, '[redacted]').slice(-4000)}`));
    });
  });
}

function decodedAttribute(html, name) {
  const match = html.match(new RegExp(`\\b${name}="([^"]*)"`));
  return match ? decodeURIComponent(match[1]) : '';
}

await mkdir(artifactDir, { recursive: true });
const base = new URL(startURL);
const token = base.searchParams.get('token');
if (!token) throw new Error('The test start URL is missing its temporary token');
const origin = base.origin;
const views = [
  { mode: 'chat', url: `${origin}/?token=${encodeURIComponent(token)}&smoke=chat`, file: 'chat.png' },
  { mode: 'settings', url: `${origin}/?token=${encodeURIComponent(token)}&smoke=settings`, file: 'settings.png' },
  { mode: 'rules', url: `${origin}/?token=${encodeURIComponent(token)}&smoke=rules`, file: 'rules.png' },
  { mode: 'live', url: `${origin}/live.html?agent=codex&name=Codex&token=${encodeURIComponent(token)}&smoke=live`, file: 'live.png' },
];

for (const view of views) {
  const userDataDir = await mkdtemp(path.join(os.tmpdir(), 'ai-agent-room-edge-'));
  const screenshot = path.join(artifactDir, view.file);
  try {
    const { stdout } = await runEdgeScreenshot([
      '--headless=new', '--disable-gpu', '--in-process-gpu', '--disable-background-networking', '--no-first-run',
      '--no-default-browser-check', '--hide-scrollbars', '--enable-logging=stderr', '--log-level=0', '--window-size=1440,900',
      '--timeout=5000', `--user-data-dir=${userDataDir}`,
      `--screenshot=${screenshot}`, '--dump-dom', view.url,
    ]);
    const ready = decodedAttribute(stdout, 'data-ui-smoke-ready');
    if (ready !== view.mode) throw new Error(`Edge did not render the ${view.mode} view`);
    const errors = decodedAttribute(stdout, 'data-ui-smoke-errors');
    if (errors) throw new Error(`${view.mode} JavaScript errors: ${errors}`);
    const check = JSON.parse(decodedAttribute(stdout, 'data-ui-smoke-check') || 'false');
    if (!check || Object.values(check).some((value) => value !== true)) {
      throw new Error(`${view.mode} UI check failed: ${JSON.stringify(check)}`);
    }
    const image = await stat(screenshot);
    if (image.size < 1000) throw new Error(`${view.mode} screenshot is empty`);
  } finally {
    await rm(userDataDir, { recursive: true, force: true, maxRetries: 3, retryDelay: 250 });
  }
}

console.log(`UI screenshots saved in ${artifactDir}: chat.png, settings.png, rules.png, live.png`);
