#!/usr/bin/env bash
set -euo pipefail
set -x

cd "$HOME"
if [[ ! -d repo ]] ; then
    git clone git@github.com:tailscale/tmemes repo
else
    git -C repo pull --rebase
fi

# Build and link a new binary.
bin="tmemes-$(date +%Y%m%d%H%M%S).bin"
( cd repo && go build -o "../${bin}" ./tmemes )
ln -s -f "$bin" ./tmemes
