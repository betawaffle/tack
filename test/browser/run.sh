#!/bin/bash
# Tests the page in headless Chromium against a fake provider. It uses a temporary
# data directory and a fake key, so it never touches real data or spends money.
set -u
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
pids=()
cleanup() {
	[ ${#pids[@]} -gt 0 ] && kill "${pids[@]}" 2>/dev/null
	rm -rf "$tmp"
}
trap cleanup EXIT

(cd "$here/../.." && go build -o "$tmp/tack" . && go build -o "$tmp/toolserver" ./test/browser/toolserver) || exit 1

printf 'sk-or-fake-key-for-browser-tests\n' >"$tmp/key"
chmod 600 "$tmp/key"

cd "$here"
mkdir -p shots "$tmp/work"
printf 'hello from the test file\n' >"$tmp/work/hello.txt"
printf 'private\n' >"$tmp/work/private.txt"
chmod 600 "$tmp/work/private.txt"

export TACK_TEST_KEY="$tmp/key" TACK_TEST_WORKDIR="$tmp/work" TACK_TEST_CONFIG="$tmp/config.json"
node fake.js >"$tmp/fake.log" 2>&1 &
pids+=($!)
cat >"$tmp/config.json" <<CONFIG
{
  "addr": "127.0.0.1:18601",
  "data": "$tmp/data",
  "key": "$tmp/key",
  "url": "http://127.0.0.1:18600/v1",
  "prompt": "$tmp/prompt.json",
  "budget_usd": 1
}
CONFIG
"$tmp/tack" -config "$tmp/config.json" >"$tmp/tack.log" 2>&1 &
pids+=($!)
"$tmp/toolserver" -addr 127.0.0.1:18602 -data "$tmp/tools" -url http://127.0.0.1:18600/v1 >"$tmp/tools.log" 2>&1 &
pids+=($!)

node check.js
status=$?
for check in check-tools.js check-start.js check-requests.js; do
	node "$check"
	result=$?
	[ $status -eq 0 ] && status=$result
done
[ $status -eq 0 ] || tail -n 5 "$tmp/tack.log" "$tmp/tools.log" "$tmp/fake.log"
exit $status
