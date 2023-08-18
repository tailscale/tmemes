#!/usr/bin/env bash
#
# Build the SQLite3 CLI from source for linux/amd64.
#
set -euo pipefail

here="$(dirname ${BASH_SOURCE[0]})"
cd "$here/.."

# Find the latest release version from the SQLite download page.  The easiest
# way seems to be to grab the embedded product data.
base='https://www.sqlite.org'
latest="$(
  curl -s $base/download.html | \
    grep ^PRODUCT | grep sqlite-autoconf- | \
    cut -d, -f3
)"
year="$(echo "$latest" | cut -d/ -f1)"
vers="$(echo "$latest" | cut -d- -f3 | cut -d. -f1)"

img=sqlite:latest
plat=linux/amd64
out=./sqlite3-"$vers"
dl="$base/$latest"

if [[ -z "$(docker image ls -q "$img")" ]] ; then
    echo "-- Fetching and building $dl ..." 1>&2
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
