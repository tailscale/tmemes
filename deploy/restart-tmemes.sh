#!/usr/bin/env bash
set -euo pipefail

lbin="$(ls -1 tmemes-*.bin | tail -1)"
svc=tmemes.service

sudo setcap CAP_NET_BIND_SERVICE=+eip $lbin
sudo cp -f "$svc" /etc/systemd/system/"$svc"

ln -sf $lbin tmemes

sudo systemctl daemon-reload
sudo systemctl enable "$svc"
sudo systemctl restart "$svc"

# Clean up old binary versions.
keep=5
ls -1 | grep -E 'tmemes-[0-9]{14}.bin' | \
    jq -sRr 'rtrimstr("\n") | split("\n")[:-'"$keep"'][]' | \
    while read -r old ; do
        printf " * cleanup: %s\n" "$old" 1>&2
        rm -f -- "$old"
    done
