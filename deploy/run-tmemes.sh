#!/usr/bin/env bash
set -euo pipefail

data="$HOME/data"

mkdir -p "$data"
"$HOME/tmemes" \
	-admin=maisem@tailscale.com,fromberger@tailscale.com \
	-allow-anonymous=false \
	-cache-max-access-age=96h \
	-cache-min-prune-mib=8192 \
	-max-image-size=16 \
	-store "$data"
