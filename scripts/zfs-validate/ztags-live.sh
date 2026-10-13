#!/usr/bin/env bash
# Live ZFS validation for the zfs_native_tags store + the #15 event cursor
# (zfs-meta host). Conventions copied from scripts/zfs-validate/run-zfs-validation.sh:
# bracketed pkill patterns, setsid starter scripts, fresh scratch dataset,
# staging-dir binaries, exact-tally acceptance, scratch destroyed after.
set -euo pipefail

HOST=zfs-meta
REMOTE_DIR='$HOME/zeta-validate'
DATASET=testpool/zval
PORT=9707
ZPORT=9708

echo "== 0. deploy + fresh scratch dataset =="
ssh -o BatchMode=yes "$HOST" '
pkill -f "[.]/zeta-serve[r]" 2>/dev/null; pkill -f "zmeta[d].*zeta-validate" 2>/dev/null
sleep 0.5
sudo zfs destroy '"$DATASET"' 2>/dev/null || true
sudo zfs create -o events=on -o events_size=1M '"$DATASET"'
sudo zfs set events=on '"$DATASET"'
sudo chown -R $(id -u):$(id -g) /'"$DATASET"'
zfs list -H -o name '"$DATASET"'
'

echo "== 1. zmetad (feature branch, layout 9) =="
ssh -o BatchMode=yes "$HOST" '
pkill -f "zmeta[d].*zeta-validate" 2>/dev/null || true
sleep 0.3
rm -f ~/zeta-validate/zmetad.db ~/zeta-validate/zmetad.db-wal ~/zeta-validate/zmetad.db-shm
setsid nohup zmetad --foreground -d ~/zeta-validate/zmetad.db -i 2 < /dev/null >> ~/zeta-validate/zmetad.log 2>&1 &
for i in $(seq 1 50); do
  [ -f ~/zeta-validate/zmetad.db ] && echo ZMETAD_UP && exit 0
  sleep 0.2
done
echo ZMETAD_FAILED; tail -20 ~/zeta-validate/zmetad.log; exit 1
'

echo "== 2. config: dataset-buckets + zfs_native_tags phase =="
ssh -o BatchMode=yes "$HOST" 'cat > ~/zeta-validate/config-ztags.json <<CFG
{
  "dataDir": "/testpool/",
  "listenAddr": ":'"$PORT"'",
  "certFile": "cert.pem",
  "keyFile": "key.pem",
  "zfs_versioning": "snapshots",
  "zfs_bucket_datasets": true,
  "zfs_native_tags": true,
  "zfs_binary": "zfs",
  "zmetad_db_path": "/home/caimlas/zeta-validate/zmetad.db",
  "zmetad_binary": "/usr/local/sbin/zmetad",
  "identities": [
    { "name": "val", "accessKey": "valuser", "secretKey": "valpass", "grants": { "*": "readwrite" } }
  ],
  "frontends": []
}
CFG'
ssh -o BatchMode=yes "$HOST" 'cat > ~/zeta-validate/start-ztags.sh <<EOF
#!/usr/bin/env bash
cd /home/caimlas/zeta-validate
PIDS=\$(pgrep -af "zeta-serve[r]" | awk "{print \$1}")
[ -n "\$PIDS" ] && kill \$PIDS 2>/dev/null
sudo -n pkill -f "[.]/zeta-serve[r]" 2>/dev/null || true
sleep 0.5
export ZETAOBJECT_CONFIG=/home/caimlas/zeta-validate/config-ztags.json
setsid nohup sudo -n -E ./zeta-server < /dev/null >> ztags-server.log 2>&1 &
echo \$! > ztags-server.pid
for i in \$(seq 1 60); do
  if timeout 2 bash -c "echo > /dev/tcp/127.0.0.1/$PORT" 2>/dev/null; then
    echo SERVER_UP; exit 0
  fi
  sleep 0.2
done
echo SERVER_FAILED; tail -30 ztags-server.log; exit 1
EOF
bash ~/zeta-validate/start-ztags.sh'

echo "== 3. cursor live asserts (since-id on ?events) + tag CLI round-trip + gateway tagging e2e =="
scp -q "$(dirname "$0")/ztags-checks.py" "$HOST:/home/caimlas/zeta-validate/ztags-checks.py"
ssh -o BatchMode=yes "$HOST" 'cd ~/zeta-validate && python3 ztags-checks.py'
RC=$?

echo "== 4. gateway tagging e2e section against the live server =="
# The live phase server answers S3 on :PORT with self-signed certs; the
# ztags asserts in ztags-checks.py already cover the wire round-trip.
# The e2e-case-shaped section runs HERE (locally) against the live host
# via a temporary AWS endpoint override:
#   (run locally, not in this script — see ztags-live-e2e.sh)

echo "== 5. teardown: stop server + zmetad, destroy scratch dataset =="
ssh -o BatchMode=yes "$HOST" '
if [ -f ~/zeta-validate/ztags-server.pid ]; then
  kill $(cat ~/zeta-validate/ztags-server.pid) 2>/dev/null || sudo -n kill $(cat ~/zeta-validate/ztags-server.pid) 2>/dev/null || true
fi
sleep 0.5
sudo -n pkill -f "[.]/zeta-serve[r]" 2>/dev/null || true
pkill -f "zmeta[d].*zeta-validate" 2>/dev/null || true
sudo zfs destroy '"$DATASET"' 2>/dev/null || true
zfs list -H -o name '"$DATASET"' 2>/dev/null && echo "DATASET_STILL_EXISTS" || echo "DATASET_DESTROYED"
'
exit $RC
