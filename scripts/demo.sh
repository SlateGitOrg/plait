#!/usr/bin/env bash
# Offline end-to-end demo: runs the fake cloud locally, then drives plait
# through plan -> apply -> no-op -> update -> drift -> crash + recovery ->
# destroy using the bundled examples/webstack configuration.
set -euo pipefail
cd "$(dirname "$0")/.."
PLAIT="$PWD/bin/plait"
WORK="$PWD/demo-work"
rm -rf "$WORK"
mkdir -p "$WORK"
cp examples/webstack/*.plait "$WORK/"

step() { printf '\n==> %s\n' "$*"; }
run() { printf '$ plait %s\n' "$*"; "$PLAIT" "$@"; }

"$PLAIT" fakecloud -addr 127.0.0.1:0 >"$WORK/fakecloud.log" 2>&1 &
FC_PID=$!
cleanup() { kill "$FC_PID" 2>/dev/null || true; wait "$FC_PID" 2>/dev/null || true; }
trap cleanup EXIT
for _ in $(seq 1 50); do
  grep -q listening "$WORK/fakecloud.log" 2>/dev/null && break
  sleep 0.1
done
URL=$(sed -n 's/^fakecloud listening on //p' "$WORK/fakecloud.log")
api() { curl -sf -X "$1" "$URL$2" ${3:+-H 'Content-Type: application/json' -d "$3"}; }
echo "fake cloud running at $URL"
printf 'cloud_endpoint = "%s"\n' "$URL" >"$WORK/plait.plaitvars"
cd "$WORK"

step "validate the configuration"
run validate

step "plan and save the plan"
run plan -out demo.plan.json

step "apply exactly the saved plan"
run apply demo.plan.json
printf '\n$ cat out/inventory.ini\n'
cat out/inventory.ini

step "plan again: nothing to do"
run plan

step "scale out and resize in one change"
run apply -auto-approve -var web_count=3 -var web_size=medium

step "drift: delete a bucket behind plait's back; apply notices and recreates it"
BUCKET_ID=$("$PLAIT" state show 'fakecloud_bucket.data["logs"]' | sed -n 's/^ *id *= "\(.*\)"/\1/p')
api DELETE "/v1/buckets/$BUCKET_ID" >/dev/null
echo "(deleted bucket $BUCKET_ID directly through the fake cloud API)"
run apply -auto-approve -var web_count=3 -var web_size=medium

step "crash: kill -9 plait in the middle of an apply"
# 400ms of API latency makes each create a wide window; with
# -parallelism 1 the kill lands while the second new instance is in flight.
api POST /v1/_faults '{"latency_ms": 400}' >/dev/null
"$PLAIT" apply -auto-approve -refresh=false -parallelism 1 -var web_count=5 -var web_size=medium >crash.log 2>&1 &
APPLY_PID=$!
sleep 0.7
kill -9 "$APPLY_PID"
wait "$APPLY_PID" 2>/dev/null || true
sleep 0.5 # the fake cloud finishes the request the dead client had already sent
api POST /v1/_faults '{}' >/dev/null
echo "killed plait (pid $APPLY_PID) mid-apply; its last progress lines:"
tail -n 3 crash.log
echo
echo "state after the crash (a stale lock is left behind):"
ls plait.state.json.lock
grep -oE '"status": "(ready|pending|tainted)"' plait.state.json | sort | uniq -c
echo "instances on the fake cloud: $(api GET /v1/instances | grep -o '"id":' | wc -l | tr -d ' ')"

step "recover: the stale lock is broken, refresh finds the in-flight object by its idempotency token"
run apply -auto-approve -var web_count=5 -var web_size=medium

step "outputs"
run output

step "destroy everything"
run destroy -auto-approve -var web_count=5 -var web_size=medium
printf '\n$ curl %s/v1/_stats\n' "$URL"
api GET /v1/_stats
echo
cd ..
rm -rf "$WORK"
