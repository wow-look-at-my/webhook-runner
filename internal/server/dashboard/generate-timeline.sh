#!/bin/sh
set -eu
mkdir -p .cache
curl -fsSL 'https://dl.pazer.build/ts0?v=10&os=linux&arch=amd64' -o .cache/ts0.cjs
node .cache/ts0.cjs build
