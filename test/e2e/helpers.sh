# Shell functions for the steps of .github/workflows/e2e.yml. Each step
# runs in its own shell, so each step that uses them sources this file.

PODS="rfr-test-no-sentinel-0 rfr-test-no-sentinel-1"

# redis_cli runs redis-cli in a pod. A Redis that does not answer makes
# redis-cli wait forever, so the timeout lets the checks fail.
# Usage: redis_cli POD ARGS...
redis_cli() {
  local pod=$1
  shift
  timeout 10 kubectl exec "$pod" -- redis-cli "$@"
}

# role prints "master" or "slave", or nothing when the pod does not answer.
role() {
  redis_cli "$1" INFO replication 2>/dev/null | sed -n 's/^role:\([a-z]*\).*/\1/p'
}

# master_pod fails unless exactly one pod answers as master, because the
# steps compare that pod with the master Service.
master_pod() {
  local pod masters=()
  for pod in $PODS; do
    if [[ "$(role "$pod")" == master ]]; then masters+=("$pod"); fi
  done
  if (( ${#masters[@]} != 1 )); then
    echo "✗ Expected exactly 1 master, found ${#masters[@]}: ${masters[*]}" >&2
    return 1
  fi
  echo "${masters[0]}"
}

is_master() {
  [[ "$(role "$1")" == master ]]
}

other_pod() {
  local pod
  for pod in $PODS; do
    if [[ "$pod" != "$1" ]]; then echo "$pod"; fi
  done
}

dbsize() {
  redis_cli "$1" DBSIZE 2>/dev/null | tr -dc '0-9'
}

# has_test_data checks the 100 keys of the "Write test data" step. No other
# client writes before the demotion test.
has_test_data() {
  [[ "$(dbsize "$1")" == 100 ]]
}

# wait_until runs a command every 2 seconds until it succeeds. On timeout it
# prints the message and fails, so the step fails.
# Usage: wait_until SECONDS MESSAGE COMMAND [ARGS...]
wait_until() {
  local end=$((SECONDS + $1)) msg=$2
  shift 2
  until "$@"; do
    if (( SECONDS >= end )); then
      echo "✗ $msg"
      return 1
    fi
    sleep 2
  done
}
