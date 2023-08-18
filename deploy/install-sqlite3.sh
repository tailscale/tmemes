#!/usr/bin/env bash
#
# Build the SQLite3 CLI from source for linux/amd64.
#
set -euo pipefail

here="$(dirname ${BASH_SOURCE[0]})"
cd "$here/.."

set -x

year="${1:-2023}"
vers="${2:-3420000}"

img=sqlite:latest
plat=linux/amd64
out=./sqlite3-"$vers"

dl="https://www.sqlite.org/${year}/sqlite-autoconf-${vers}.tar.gz"

if [[ -z "$(docker image ls -q "$img")" ]] ; then
    cat <<EOF | docker build -t sqlite:latest -
FROM --platform="$plat" ubuntu:20.04 as builder

WORKDIR /root

RUN apt-get update && apt-get install -y build-essential file libreadline-dev zlib1g-dev curl
RUN curl -sL "$dl" | tar xzv
RUN cd sqlite-autoconf-"$vers" && ./configure --enable-readline && make -j 
EOF
fi

c="$(docker create --platform=$plat $img)"
trap "docker rm $c" EXIT
mkdir -p "$(dirname "$out")"
docker cp "$c":/root/sqlite-autoconf-"$vers"/sqlite3 "$out"
chmod +x "$out"
