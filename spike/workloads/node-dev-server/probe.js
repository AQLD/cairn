// Ask the dev server for its state over loopback, from inside the sandbox.
fetch("http://127.0.0.1:3000/")
  .then((r) => r.text())
  .then((t) => process.stdout.write(t))
  .catch((e) => { console.error(String(e)); process.exit(1); });
