# yomi Web UI

The web UI is a Vue 3 + Vite application. The Go server embeds the generated
`../dist` directory, so the build output is intentionally kept in the
repository while `node_modules` remains local-only.

```bash
npm install
npm run build
```

During the incremental migration, `src/legacy.js` owns the existing chat and
Trace behavior. New screens should be added as Vue components and gradually
move state out of the compatibility controller.
