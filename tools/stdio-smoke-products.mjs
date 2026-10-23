// Product fixtures for the optional real-server smoke test.
// Keep the existing entry arguments and isolated server settings compatible.
import { resolve } from 'node:path'

export function smokeProducts(args) {
  const [binaryArg, artifactArg, approvalArg, aitermArg] = args
  if (![binaryArg, artifactArg, approvalArg, aitermArg].every(Boolean)) {
    throw Error('usage: node real-stdio-smoke.mjs BINARY ARTIFACT_DIR APPROVAL_BOX_CLI AITERM_CLI')
  }
  return {
    binaryArg,
    artifactArg,
    products: [
      ['approval-box', resolve(approvalArg), ['mcp']],
      ['aiterm', resolve(aitermArg), []],
    ],
    serverEnvironment: dir => ({
      APPROVAL_BOX_SERVER: 'http://127.0.0.1:9',
      AITERM_STATE_BASE: dir + '/aiterm-state',
    }),
    wakeMarker: dir => dir + '/aiterm-state',
  }
}
