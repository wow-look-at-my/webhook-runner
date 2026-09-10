// INTERIM type shim for the buildhost-served <perf-graph> module, the same
// stopgap as js-snippets-activity-feed.d.ts next to it. The import in
// ts/stats.ts is SIDE-EFFECT ONLY: it registers the element, and this bundle
// drives instances through the PerfGraphLike interface declared there.
declare module 'https://sites.pazer.build/js-snippets/branch/library/ui/perf-graph.js';
