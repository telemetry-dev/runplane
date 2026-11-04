#!/usr/bin/env bash
# Fake GitHub Actions runner for runplane tests. It gets its jobs from fakegh
# (internal/github/fakegh) and prints the status lines of the real runner,
# which runplane-init reads. It runs on bash 3.2 (macOS) and needs curl and jq.
# bin/Runner.Worker.dll holds only the text that the runplane runner patch
# changes, so that the bundle counts as patched.
#
# Usage: ./run.sh --jitconfig <encoded JIT config>
set -uo pipefail

cd "$(dirname "$0")" || exit 1

jitconfig=
while [ $# -gt 0 ]; do
  if [ "$1" = --jitconfig ] && [ $# -gt 1 ]; then
    jitconfig=$2
    shift
  fi
  shift
done
if [ -z "$jitconfig" ]; then
  echo "Not configured. Run ./run.sh --jitconfig <config>." >&2
  exit 1
fi

# The JIT config is base64 JSON that maps file names to base64 file contents.
config_file() {
  printf '%s' "$jitconfig" | base64 -d | jq -er --arg f "$1" '.[$f]' | base64 -d
}
if ! runner=$(config_file .runner) || ! credentials=$(config_file .credentials); then
  echo "The JIT config is not valid." >&2
  exit 1
fi
url=$(jq -r .serverUrl <<<"$runner")
id=$(jq -r .agentId <<<"$runner")
token=$(jq -r .data.token <<<"$credentials")

now() { date -u '+%Y-%m-%d %H:%M:%SZ'; }
logtime() { date -u '+%Y-%m-%dT%H:%M:%S.0000000Z'; }

tmp=$(mktemp -d)
child=
beat=
stopping=
canceled=
completed=

# shellcheck disable=SC2329 # the EXIT trap calls it
on_exit() {
  [ -n "$beat" ] && kill "$beat" 2>/dev/null
  [ -z "$completed" ] && curl -sS --max-time 5 -o /dev/null -X DELETE \
    -H "Authorization: Bearer $token" "$url/_fake/runners/$id/session" 2>/dev/null
  rm -rf "$tmp"
}
trap on_exit EXIT
# Signals stop the command that runs in the background, so that the script
# reacts at once and not after a long poll or a sleep.
trap 'stopping=1; [ -n "$child" ] && kill "$child" 2>/dev/null' TERM INT
trap 'canceled=1; [ -n "$child" ] && kill "$child" 2>/dev/null' USR1

# background runs a command so that a trap can stop it, and returns its
# exit code, or a code above 128 if a signal came.
background() {
  "$@" &
  child=$!
  wait "$child"
  local code=$?
  kill "$child" 2>/dev/null
  wait "$child" 2>/dev/null
  child=
  return "$code"
}

# request METHOD PATH [JSON] calls the runner API of fakegh. It sets status
# to the HTTP status (000 when the call failed) and writes the body to
# $tmp/body.
request() {
  background curl -sS --max-time 60 -o "$tmp/body" -w '%{http_code}' -X "$1" \
    -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    ${3:+--data "$3"} "$url/_fake/runners/$id$2" >"$tmp/status" 2>/dev/null
  status=$(cat "$tmp/status")
  [ -n "$status" ] || status=000
}

mkdir -p _diag/pages
diag="_diag/Runner_$(date -u '+%Y%m%d-%H%M%S')-utc.log"
diaglog() { echo "[$(now) INFO Runner] $*" >>"$diag"; }

# Connect. The real runner tries again while GitHub cannot be reached.
while :; do
  [ -n "$stopping" ] && exit 0
  request POST /session
  case $status in
    200) break ;;
    000 | 5*) background sleep 1 ;;
    *)
      echo "$(now): The runner registration was deleted from the server ($status)."
      exit 1
      ;;
  esac
done
heartbeat=$(jq -r .heartbeat "$tmp/body")
diaglog "Connected to $url as runner $id"
echo
echo "√ Connected to GitHub"
echo
echo "Current runner version: '2.330.0'"
echo "$(now): Listening for Jobs"

# The heartbeat keeps a busy runner alive and passes a cancel on as USR1.
# It does not hold the output open after the runner exits.
main=$$
(
  while kill -0 "$main" && sleep "$heartbeat"; do
    out=$(curl -sS --max-time 10 -X POST -H "Authorization: Bearer $token" \
      "$url/_fake/runners/$id/heartbeat")
    if [ "$(jq -r '.canceled // false' <<<"$out")" = true ]; then
      kill -USR1 "$main"
    fi
  done
) >/dev/null 2>&1 &
beat=$!

while :; do
  if [ -n "$stopping" ]; then
    echo "Exiting..."
    exit 0
  fi
  request GET /job
  case $status in
    200) break ;;
    204) ;;
    000 | 5*) background sleep 1 ;;
    *)
      echo "$(now): The runner registration was deleted from the server ($status)."
      exit 1
      ;;
  esac
done
job="$tmp/job"
cp "$tmp/body" "$job"
name=$(jq -r .name "$job")
request_id=$(jq -r .request_id "$job")
timeline=$(jq -r .timeline_id "$job")
[ "$(jq -r .canceled "$job")" = true ] && canceled=1
echo "$(now): Running job: $name"
diaglog "Running job: $name"

# Run the steps. Each step writes its log page, as the real runner does. A
# page name is <timeline>_<record>_<page>.log, and record 1 is "Set up job".
result=Succeeded
steps=$(jq '.steps | length' "$job")
i=0
while [ "$i" -lt "$steps" ] && [ -z "$stopping$canceled" ]; do
  # The real runner writes a page through one open file, and runplane-init
  # stops to read a page when its writer closes it.
  exec 3>>"_diag/pages/${timeline}_$((i + 2))_1.log"
  echo "$(logtime) ##[group]$(jq -r ".steps[$i].name" "$job")" >&3
  echo "$(logtime) ##[endgroup]" >&3
  jq -r ".steps[$i].lines[]?" "$job" | while IFS= read -r line; do
    echo "$(logtime) $line"
  done >&3
  seconds=$(jq -r ".steps[$i].sleep // 0" "$job")
  [ "$seconds" = 0 ] || background sleep "$seconds" 3>&-
  failed=$(jq -r ".steps[$i].fail // false" "$job")
  command=$(jq -r ".steps[$i].run // empty" "$job")
  if [ -n "$command" ]; then
    background bash -c "$command" >"$tmp/out" 2>&1 3>&- || failed=true
    while IFS= read -r line; do
      echo "$(logtime) $line"
    done <"$tmp/out" >&3
  fi
  if [ "$failed" = true ]; then
    echo "$(logtime) ##[error]Process completed with exit code 1." >&3
    result=Failed
  fi
  exec 3>&-
  [ "$result" = Failed ] && break
  i=$((i + 1))
done
[ -n "$stopping$canceled" ] && result=Canceled

echo "$(now): Job $name completed with result: $result"
diaglog "Job $name completed with result: $result"
body=$(jq -cn --argjson id "$request_id" --arg result "$result" '{request_id: $id, result: $result}')
for _ in 1 2 3 4 5; do
  request POST /complete "$body"
  case $status in
    000 | 5*) background sleep 1 ;;
    *) break ;;
  esac
done
completed=1
exit 0
