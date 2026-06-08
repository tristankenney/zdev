#!/usr/bin/env node
// Contract test for config/opencode/zdev-notify.js — drives the plugin
// with synthetic opencode events against a stubbed Bun-$ and asserts
// the zdev-notify invocations. Runs under plain node (no bun, no
// opencode) so CI can gate the adapter without an agent install.
//
//   node scripts/test-opencode-plugin.mjs
//
// Exits non-zero with a diff on any mismatch.
import { ZdevNotify } from "../config/opencode/zdev-notify.js"
import { fileURLToPath } from "node:url"
import path from "node:path"

process.chdir(path.dirname(fileURLToPath(import.meta.url)))

const calls = []
// Stub of Bun's $ tagged template: records the interpolated argv and
// returns a thenable with .quiet() like the real one.
const $ = (strings, ...values) => {
  const cmd = strings
    .reduce((acc, s, i) => acc + s + (i < values.length ? values[i] : ""), "")
    .trim()
  const p = Promise.resolve()
  p.quiet = () => {
    calls.push(cmd)
    return Promise.resolve()
  }
  return p
}

const hooks = await ZdevNotify({ $ })
if (typeof hooks.event !== "function") {
  console.error("FAIL: plugin must export an `event` hook (opencode's catch-all)")
  process.exit(1)
}

const fire = (type, properties) => hooks.event({ event: { type, properties } })

await fire("session.idle")
await fire("permission.asked")
await fire("session.error")
await fire("message.updated") // noise — must NOT notify
await fire("storage.write") // noise — must NOT notify
await fire("tui.command.execute", { command: "prompt.submit" }) // → clear
await fire("tui.command.execute", { command: "other" }) // noise — must NOT notify

const home = process.env.HOME
const want = [
  `${home}/.local/bin/zdev-notify opencode alive`,
  `${home}/.local/bin/zdev-notify opencode done`,
  `${home}/.local/bin/zdev-notify opencode needs-permission`,
  `${home}/.local/bin/zdev-notify opencode waiting`,
  `${home}/.local/bin/zdev-notify opencode clear`,
]
const got = calls
if (JSON.stringify(got) !== JSON.stringify(want)) {
  console.error("FAIL: zdev-notify invocations mismatch")
  console.error("  want:", want)
  console.error("  got: ", got)
  process.exit(1)
}

// The notify wrapper must never throw into the agent: simulate a
// failing shellout and re-fire.
const $boom = () => {
  const p = Promise.resolve()
  p.quiet = () => Promise.reject(new Error("zdev-notify missing"))
  return p
}
const hooksBoom = await ZdevNotify({ $: $boom })
try {
  await hooksBoom.event({ event: { type: "session.idle" } })
} catch (e) {
  console.error("FAIL: a failing zdev-notify must not throw into opencode:", e)
  process.exit(1)
}

console.log("ok: opencode plugin contract (alive+clear mapped, 3 states mapped, noise ignored, failures swallowed)")
