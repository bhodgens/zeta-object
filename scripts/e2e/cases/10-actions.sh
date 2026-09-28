# 10-actions.sh — .bucket-actions after_upload action with a SAFE command
# (touch a sentinel file using $FILE_PATH, shell-quoted by the server), then
# poll up to 5s for the sentinel.
# NOTE: the inactivity-timeout trigger is timing-flaky in real time and is
# unit-covered in actions_test.go — not exercised here.
set -u
BKT='e2e-10-actions'
aws s3api create-bucket --bucket "$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1

# sentinel dir is a per-suite tmpdir (E2E_SENTINEL_DIR from run-e2e.sh)
SENT="$E2E_SENTINEL_DIR/10-upload-fired"
rm -f "$SENT"

# The server shell-quotes $FILE_PATH itself, so reference it unquoted here.
cat > "$E2E_DATA_DIR/$BKT/.bucket-actions" <<EOF
{
  "version": "1",
  "after_upload": [
    {
      "name": "touch-sentinel",
      "patterns": ["*"],
      "command": "touch $SENT"
    }
  ]
}
EOF

TMP=$(mktemp /tmp/e2e10.XXXXXX)
printf 'fire-the-action\n' > "$TMP"
aws s3 cp "$TMP" "s3://$BKT/trigger.txt" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1
assert_eq 'upload for action trigger exit 0' 0 $?

# poll up to 5s for the sentinel
FIRED=0
for _ in 1 2 3 4 5 6 7 8 9 10; do
	if [ -e "$SENT" ]; then
		FIRED=1
		break
	fi
	sleep 0.5
done
assert_eq 'after_upload action fired (sentinel touched)' 1 "$FIRED"

rm -f "$TMP" "$SENT" "$E2E_DATA_DIR/$BKT/.bucket-actions"
aws s3 rb "s3://$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl --force >/dev/null 2>&1
