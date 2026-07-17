#!/bin/sh
set -eu
mkdir -p .cache
curl -fsSL 'https://dl.pazer.build/ts0?v=2&os=linux&arch=amd64' -o .cache/ts0.cjs
curl -fsSL 'https://wow-look-at-my.github.io/js-snippets/ui/timeline-view.d.ts' -o ts/js-snippets/timeline-view.d.ts
curl -fsSL 'https://wow-look-at-my.github.io/js-snippets/ui/timeline-view-math.d.ts' -o ts/js-snippets/timeline-view-math.d.ts
node .cache/ts0.cjs build
