#!/bin/sh
# Installs gru from its latest release, or --version vX.Y.Z, into --prefix DIR (~/.local):
#   curl -fsSL https://raw.githubusercontent.com/andres-nav/minion/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/andres-nav/minion/main/install.sh | sh -s -- --version v0.2.0
# gru upgrade updates it later. GRU_RELEASES overrides the release API, for tests.
set -eu
release=latest prefix=$HOME/.local
while [ $# -gt 0 ]; do
  case $1 in
    --version) release=tags/$2 ;;
    --prefix) prefix=$2 ;;
    *) echo "install.sh: unknown option $1" >&2 && exit 2 ;;
  esac
  shift 2
done
os=$(uname -s | tr A-Z a-z)
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "install.sh: no gru for $(uname -m)" >&2 && exit 1 ;;
esac
a=gru-$os-$arch.tar.gz
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
curl -fsSL "${GRU_RELEASES:-https://api.github.com/repos/andres-nav/minion/releases}/$release" |
  tr , '\n' | sed -n 's/.*"browser_download_url": *"\([^"]*\)".*/\1/p' > "$t/urls"
for f in "$a" checksums.txt; do
  u=$(grep "/$f\$" "$t/urls") || { echo "install.sh: the release has no $f" >&2 && exit 1; }
  curl -fsSL -o "$t/$f" "$u"
done
sum=sha256sum
command -v sha256sum > /dev/null || sum='shasum -a 256'
(cd "$t" && grep " $a\$" checksums.txt | $sum -c > /dev/null) ||
  { echo "install.sh: $a doesn't match the release's checksums.txt" >&2 && exit 1; }
mkdir "$t/gru" && tar -xzf "$t/$a" -C "$t/gru"
"$t/gru/gru" install --prefix "$prefix"
