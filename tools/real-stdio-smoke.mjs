// Read-only protocol smoke test. No tools are invoked and the state is isolated.
// node tools/real-stdio-smoke.mjs BINARY ARTIFACT_DIR APPROVAL_BOX_CLI AITERM_CLI
import { spawn } from 'node:child_process'
import { createHash } from 'node:crypto'
import { mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs'
import { resolve } from 'node:path'
import assert from 'node:assert/strict'

const [binaryArg, artifactArg, approvalArg, aitermArg] = process.argv.slice(2)
if (![binaryArg, artifactArg, approvalArg, aitermArg].every(Boolean)) {
  throw Error('usage: node real-stdio-smoke.mjs BINARY ARTIFACT_DIR APPROVAL_BOX_CLI AITERM_CLI')
}
const binary = resolve(binaryArg)
const artifacts = resolve(artifactArg)
mkdirSync(artifacts, { recursive: true })
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms))
const init = {
  protocolVersion: '2025-11-25',
  capabilities: { roots: { listChanged: true }, elicitation: { form: {}, url: {} } },
  clientInfo: {
    name: 'claude-code', title: 'Claude Code', version: '2.1.289',
    description: "Anthropic's agentic coding tool", websiteUrl: 'https://claude.com/claude-code',
  },
}
const discovery = { _meta: {
  'io.modelcontextprotocol/protocolVersion': '2026-07-28',
  'io.modelcontextprotocol/clientInfo': init.clientInfo,
  'io.modelcontextprotocol/clientCapabilities': init.capabilities,
} }
const digest = value => createHash('sha256').update(JSON.stringify(value)).digest('hex')
function descendants(root) {
  const parents = new Map()
  for (const name of readdirSync('/proc')) {
    if (!/^\d+$/.test(name)) continue
    try { parents.set(Number(name), Number(readFileSync('/proc/'+name+'/stat', 'utf8').split(') ')[1].split(' ')[1])) } catch {}
  }
  const result = []
  function walk(pid) { for (const [candidate, parent] of parents) if (parent === pid) { result.push(candidate); walk(candidate) } }
  walk(root)
  return result
}
function active(pid) {
  try { return readFileSync('/proc/'+pid+'/stat', 'utf8').split(') ')[1][0] !== 'Z' } catch { return false }
}
function connect(command, args, env) {
  const child = spawn(command, args, { env, stdio: ['pipe', 'pipe', 'pipe'], cwd: artifacts })
  let buffer = '', stderr = '', nextID = 0
  const pending = new Map()
  const completed = new Promise((resolve, reject) => { child.once('error', reject); child.once('exit', (code, signal) => resolve({ code, signal })) })
  child.stderr.on('data', chunk => { stderr += chunk })
  child.stdout.on('data', chunk => {
    buffer += chunk
    for (;;) {
      const at = buffer.indexOf('\n')
      if (at < 0) break
      const text = buffer.slice(0, at); buffer = buffer.slice(at+1)
      if (!text.trim()) continue
      const answer = JSON.parse(text)
      const waiting = pending.get(answer.id)
      if (waiting) { pending.delete(answer.id); waiting(answer) }
    }
  })
  const request = (method, params) => new Promise((resolve, reject) => {
    const id = ++nextID
    const timer = setTimeout(() => reject(Error(method+' timed out; '+stderr)), 10_000)
    pending.set(id, answer => { clearTimeout(timer); resolve(answer) })
    child.stdin.write(JSON.stringify({ jsonrpc: '2.0', id, method, params })+'\n')
  })
  const notify = method => child.stdin.write(JSON.stringify({ jsonrpc: '2.0', method })+'\n')
  return {
    child, request, notify,
    async close(signal = 'SIGHUP') {
      const owned = descendants(child.pid)
      child.kill(signal)
      const exited = await Promise.race([completed, sleep(3000).then(() => { throw Error('relay/server did not exit') })])
      await sleep(300)
      assert.equal(owned.filter(active).length, 0, 'descendants remain after shutdown')
      return exited
    },
  }
}
async function prepare(args, env, reportPath) {
  const child = spawn(binary, ['--check', ...args], { env, cwd: artifacts, stdio: ['ignore','pipe','pipe'] })
  let out = '', stderr = ''
  child.stdout.on('data',chunk => { out += chunk })
  child.stderr.on('data',chunk => { stderr += chunk })
  const timeout = setTimeout(() => child.kill('SIGTERM'),15_000)
  const code = await new Promise((resolve,reject) => { child.once('error',reject);child.once('exit',resolve) })
  clearTimeout(timeout)
  assert.equal(code,0,'check failed: '+stderr)
  const report = JSON.parse(out)
  assert.equal(report.beforeRequests,1)
  writeFileSync(reportPath,out)
  return report
}
const results = []
for (const [product, cli, argv] of [
  ['approval-box', resolve(approvalArg), ['mcp']],
  ['aiterm', resolve(aitermArg), []],
]) {
  const dir = artifacts+'/'+product
  mkdirSync(dir+'/home',{recursive:true})
  const env = { ...process.env,
    HOME:dir+'/home', CODEX_HOME:dir+'/home/.codex',
    CLAUDE_CONFIG_DIR:dir+'/home/.claude', CURSOR_HOME:dir+'/home/.cursor', GROK_HOME:dir+'/home/.grok',
    APPROVAL_BOX_SERVER:'http://127.0.0.1:9', AITERM_STATE_BASE:dir+'/aiterm-state',
    MCP_LAZY_CACHE_DIR:dir+'/cache', MCP_LAZY_START_TIMEOUT:'5s', MCP_LAZY_IDLE_STOP:'0',
    MCP_LAZY_CHECK_INITIALIZE:JSON.stringify(init),
    MCP_LAZY_CHECK_BEFORE:JSON.stringify([{method:'server/discover',params:discovery}]),
    MCP_LAZY_WAKE_COMMAND:'', MCP_LAZY_WAKE_INTERVAL:'5s', MCP_LAZY_WAKE_TIMEOUT:'1s',
  }
  const args = [process.execPath, cli, ...argv]
  const baseline = connect(args[0],args.slice(1),env)
  let directDiscovery, directInit, directList, directUnknown
  try {
    directDiscovery = await baseline.request('server/discover',discovery)
    directInit = await baseline.request('initialize',init)
    baseline.notify('notifications/initialized')
    directList = await baseline.request('tools/list',{})
    directUnknown = await baseline.request('smoke/unknown',{})
  } finally { await baseline.close() }
  const check = await prepare(args,env,dir+'/check.json')
  const relay = connect(binary,args,env)
  let serverCount
  try {
    const found = await relay.request('server/discover',discovery)
    assert.deepEqual(found,directDiscovery)
    assert.deepEqual(await relay.request('initialize',init),directInit)
    relay.notify('notifications/initialized')
    assert.deepEqual(await relay.request('tools/list',{}),directList)
    await sleep(150)
    serverCount = descendants(relay.child.pid).length
    assert.equal(serverCount,0,'--check-only cache did not keep first session lazy')
    assert.deepEqual(await relay.request('smoke/unknown',{}),directUnknown)
    assert.ok(descendants(relay.child.pid).length>0,'ordinary unknown request did not start server')
  } finally { await relay.close() }
  const wake = connect(binary,args,{...env,MCP_LAZY_WAKE_COMMAND:JSON.stringify(['/usr/bin/test','-d',dir+'/aiterm-state']),MCP_LAZY_WAKE_INTERVAL:'50ms'})
  try {
    await wake.request('initialize',init)
    wake.notify('notifications/initialized')
    // Match a generic condition in an isolated directory; no delivery state is injected.
    mkdirSync(dir+'/aiterm-state',{recursive:true})
    let awake=false
    for(let n=0;n<100;n++) { if(descendants(wake.child.pid).length>0){awake=true;break}; await sleep(20) }
    assert.ok(awake,'matching predicate did not start real server')
    assert.deepEqual((await wake.request('tools/list',{})).result,directList.result)
  } finally { await wake.close() }
  results.push({ product, tools:check.lists['tools/list'].count,
    discoveryDigest:digest(directDiscovery.error??directDiscovery.result),
    initializeDigest:digest(directInit.result), listingDigest:digest(directList.result),
    beforeFirstRequestServers:serverCount, checkedFirstSessionLazy:true, ordinaryRequestStartsServer:true,
    wakeStartsServer:true, sighupLeavesDescendants:0 })
}
writeFileSync(artifacts+'/results.json',JSON.stringify(results,null,2)+'\n')
console.log(JSON.stringify(results))
