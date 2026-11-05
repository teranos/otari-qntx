#!/usr/bin/env bash
# Fail when the archive is larger than the node can take, and say what in the
# tree is largest.
#
# usage: size.sh <archive> <tree> <max bytes>
#
# The node downloads a plugin's artifact within its GitHub client's 30 s
# (QNTX plugin/grpc/services/github_server.go) and holds it whole in memory
# to unzip it (server/plugin_artifacts.go). Run 41961599788's 526,284,574
# bytes stopped after 207,388,672 of them in those 30 s, about 6.9 MB/s.
# capy's archive, the largest a pyre plugin on q came in, is 116,462,153
# bytes. The bound otaripy is held to is 150,000,000: capy's size class, and
# at that rate about 22 s of the 30.

set -euo pipefail

ARCHIVE=${1:?usage: size.sh <archive> <tree> <max bytes>}
TREE=${2:?usage: size.sh <archive> <tree> <max bytes>}
MAX=${3:?usage: size.sh <archive> <tree> <max bytes>}

size=$(wc -c < "$ARCHIVE" | tr -d ' ')
echo "archive: $size bytes, bound $MAX"
echo "tree, largest parts:"
du -sk "$TREE"/* "$TREE"/python/* "$TREE"/python/lib/python3.13/site-packages/* 2>/dev/null \
    | sort -rn | head -n 20 | awk '{printf "  %8.1f MB  %s\n", $1 / 1024, $2}'
if [ "$size" -gt "$MAX" ]; then
    echo "the archive is $size bytes, over the $MAX the node takes" >&2
    exit 1
fi
