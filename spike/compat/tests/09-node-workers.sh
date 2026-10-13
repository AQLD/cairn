#!/bin/bash
# Node worker threads
set -euo pipefail
cd /tmp
node -e "
const { Worker } = require(\"worker_threads\");
let done = 0;
for (let i = 0; i < 4; i++) {
  const w = new Worker(\"const {parentPort}=require(\\\"worker_threads\\\"); let s=0; for(let j=0;j<1e7;j++) s+=j; parentPort.postMessage(s)\", { eval: true });
  w.on(\"message\", () => { if (++done === 4) console.log(\"ok\"); });
}"
