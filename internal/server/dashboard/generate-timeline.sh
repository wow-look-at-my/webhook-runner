#!/bin/sh
set -eu
mkdir -p .cache ts/js-snippets
curl -fsSL 'https://dl.pazer.build/ts0?v=2&os=linux&arch=amd64' -o .cache/ts0.cjs
curl -fsSL 'https://sites.pazer.build/js-snippets/branch/library/ui/timeline-view.d.ts' -o ts/js-snippets/timeline-view.d.ts
curl -fsSL 'https://sites.pazer.build/js-snippets/branch/library/ui/timeline-view-math.d.ts' -o ts/js-snippets/timeline-view-math.d.ts
node .cache/ts0.cjs build
