#!/usr/bin/env bash
# Teardown: stop the validation server + zmetad, destroy the scratch
# dataset and every child, verify nothing remains (gate 5).
PIDS=$(pgrep -af 'zeta-serve[r]' | awk '{print $1}')
[ -n "$PIDS" ] && kill $PIDS 2>/dev/null
sudo -n pkill -f '[.]/zeta-serve[r]' 2>/dev/null || true
pkill -f 'zmeta[d].*zeta-validate' 2>/dev/null || true
sleep 1
sudo zfs destroy testpool/zval/clidemo 2>/dev/null || true
sudo zfs destroy testpool/ztags-bucket 2>/dev/null || true
sudo zfs destroy testpool/zval 2>/dev/null || true
echo "== remaining zval/ztags datasets =="
zfs list -H -o name -r testpool 2>/dev/null | grep -E "zval|ztags" || echo "NONE (all destroyed)"
pgrep -af 'zeta-serve[r]' | grep -v grep || echo "no server processes"
pgrep -af 'zmeta[d]' | grep -v grep || echo "no zmetad processes"
