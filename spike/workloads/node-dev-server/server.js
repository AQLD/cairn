// A dev server like the ones coding agents run: serves HTTP from memory and
// watches source files for changes. After a restore, the same process must
// still serve (hit counter keeps rising) and the watcher must still fire.
const http = require("http");
const fs = require("fs");

const started = Date.now();
let hits = 0;
let reloads = 0;

fs.watch("/app/src", { recursive: true }, () => { reloads++; });

http.createServer((req, res) => {
  hits++;
  res.setHeader("content-type", "application/json");
  res.end(JSON.stringify({ pid: process.pid, started, hits, reloads, ready: hits >= 1 }));
}).listen(3000, "127.0.0.1");
