// `.css` files import as strings (esbuild's text loader, configured in
// ts0.json) — the vendored <timeline-view> adopts its stylesheet into its
// shadow root at runtime. This ambient declaration is what makes those
// imports type-check (same pattern as js-snippets' css.d.ts).
declare module '*.css' {
  const src: string;
  export default src;
}
