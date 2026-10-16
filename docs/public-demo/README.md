# AMAC delivery health lab

This browser demo runs AMAC's shared Go health evaluator against sample inputs entered in the page. It does not connect to a Mac, agent, terminal, job tracker, notification service, or external API.

Build the browser engine from the repository root:

```sh
./scripts/build-public-demo-wasm.sh
```

The script compiles `internal/health/core` for WebAssembly and copies Go's matching `wasm_exec.js` runtime into this directory. Deploy this directory as the Vercel project root. Rebuild the WASM artifact whenever the shared evaluator changes.

The form accepts up to 20 automation rows. Cadence and grace must be from 0 to 525600 minutes, with cadence above zero. Delivery age may be from -1440 to 525600 minutes, or blank if unknown. A no-op preserves the entered delivery time. Probe errors become unknown, failed runs remain failing, and results are sorted worst-first by the native evaluator.

No form inputs or reports persist after reload. The page makes no external requests except loading its own static files.
