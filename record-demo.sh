#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p recordings
export TERM=xterm-256color
script --quiet --return --flush \
  --log-out recordings/lifecycle.typescript \
  --log-timing recordings/lifecycle.timing \
  --command 'stty cols 120 rows 40; sudo ask-exec env TMPDIR=/home/amnezia/.hermes/cache/scratch bash ./demo-lifecycle.sh'
