// INTERIM type shim for the buildhost-served <activity-feed> module — the
// same stopgap, and the same eventual replacement, as
// js-snippets-timeline.d.ts next to it: once the generate step FETCHES
// upstream's declarations (the library site already serves a .d.ts beside
// every .js) into committed, freshness-gated files, an upstream API change
// turns CI red instead of drifting. This file is the stopgap, not the
// convention.
//
// The component is NOT vendored: the browser imports it at runtime from
// js-snippets' buildhost library site, and the built bundle keeps the URL
// verbatim (esbuild `external`).
//
// The import in ts/timeline.ts is SIDE-EFFECT ONLY — importing the module
// registers the <activity-feed> element, and nothing in this bundle touches
// its exports. dashboard.js (a classic script, not part of this bundle)
// feeds the elements. So this declaration deliberately declares no exports:
// adding a mirror of upstream's API here would be surface nobody consumes,
// and surface that can silently rot.
declare module 'https://sites.pazer.build/js-snippets/branch/library/ui/activity-feed.js';
