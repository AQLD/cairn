#!/bin/bash
# File watcher sees a change
set -euo pipefail
cd /tmp
mkdir w && node -e "
const fs = require(\"fs\");
fs.watch(\"/tmp/w\", () => { console.log(\"ok\"); process.exit(0); });
setTimeout(() => fs.writeFileSync(\"/tmp/w/x\", \"1\"), 200);
setTimeout(() => { console.error(\"watcher did not fire\"); process.exit(1); }, 5000);"
