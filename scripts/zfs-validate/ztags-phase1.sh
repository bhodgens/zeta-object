#!/usr/bin/env bash
# Phase-1 variant that PRE-CREATES the ztags bucket dataset with events=on
# (unprivileged `zfs create` cannot mount children on this host, so the
# harness pre-seeds it the way section 12/13 do with sudo; the server's
# PUT /ztags-bucket then finds the directory present and adopts it).
set -e
pkill -f '[.]/zeta-serve[r]' 2>/dev/null || true
pkill -f 'zmeta[d].*zeta-validate' 2>/dev/null || true
sleep 0.5
sudo zfs destroy testpool/zval 2>/dev/null || true
sudo zfs destroy testpool/ztags-bucket 2>/dev/null || true
sudo zfs create -o events=on -o events_size=1M testpool/zval
sudo zfs set events=on testpool/zval
sudo zfs create -o events=on testpool/ztags-bucket
sudo chown -R "$(id -u):$(id -g)" /testpool/zval /testpool/ztags-bucket
rm -f ~/zeta-validate/zmetad.db ~/zeta-validate/zmetad.db-wal ~/zeta-validate/zmetad.db-shm
bash ~/zeta-validate/ztags-zmetad-starter.sh
cat > ~/zeta-validate/config-ztags.json <<CFG
{
  "dataDir": "/testpool/",
  "listenAddr": ":9707",
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
  "frontends": [
    { "type": "s3" },
    { "type": "webdav", "listenAddr": "127.0.0.1:9713" }
  ]
}
CFG
bash ~/zeta-validate/start-ztags.sh
