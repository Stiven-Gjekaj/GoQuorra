#!/usr/bin/env bash
#
# Submit a job to a running stack and wait for a worker to finish it.
#
# This is the check that the parts are wired to each other. Every other test
# in this project covers one part.

set -euo pipefail

SERVER="${QUORRA_SERVER:-http://localhost:8080}"
KEY="${QUORRA_API_KEY:-local-development-key-not-for-anywhere-else}"

say() { echo "smoke: $*"; }

# Wait for the server before asking it anything, so that a slow start reads as
# a slow start rather than as a refused connection.
for _ in $(seq 1 60); do
	if curl -fsS "$SERVER/readyz" > /dev/null 2>&1; then
		break
	fi
	sleep 1
done

curl -fsS "$SERVER/readyz" > /dev/null || { say "the server never became ready"; exit 1; }
say "the server is ready"

submit() {
	curl -fsS -X POST "$SERVER/v1/jobs" \
		-H "Content-Type: application/json" \
		-H "X-API-Key: $KEY" \
		-d "$1"
}

status_of() {
	curl -fsS "$SERVER/v1/jobs/$1" -H "X-API-Key: $KEY" |
		sed -n 's/.*"status":"\([a-z]*\)".*/\1/p'
}

# A job that succeeds.
good=$(submit '{"type":"echo","payload":{"hello":"world"}}' | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
[ -n "$good" ] || { say "the server returned no identifier"; exit 1; }
say "submitted $good"

# A job that fails every time, so the retry schedule and the dead letter queue
# are covered as well. Zero retries, so it does not spend a minute in backoff.
bad=$(submit '{"type":"fail","payload":{},"max_retries":0}' | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
say "submitted $bad, which always fails"

counter() {
	curl -fsS "$SERVER/metrics" | sed -n "s/^$1 \(.*\)$/\1/p"
}

# Check that a worker is there before waiting a minute for one.
#
# Without this, a stack whose workers never started reports "the job is still
# pending" after sixty seconds, which names the symptom and not the cause. The
# first run of this script in CI failed exactly that way: both worker
# containers were running the server binary, so nothing ever leased anything,
# and the message sent the reader to look at the queue rather than at the
# containers.
for _ in $(seq 1 20); do
	leased=$(counter quorra_jobs_leased_total)
	if [ -n "$leased" ] && [ "${leased%.*}" -gt 0 ]; then
		say "a worker has leased $leased job(s)"
		break
	fi
	sleep 1
done

leased=$(counter quorra_jobs_leased_total)
if [ -z "$leased" ] || [ "${leased%.*}" -lt 1 ]; then
	say "no worker asked for work in twenty seconds, so nothing will run these jobs"
	say "check that the worker containers are running the worker binary"
	exit 1
fi

wait_for() {
	local id="$1" want="$2"
	for _ in $(seq 1 60); do
		local now
		now=$(status_of "$id")
		if [ "$now" = "$want" ]; then
			say "$id reached $want"
			return 0
		fi
		sleep 1
	done
	say "$id is $(status_of "$id"), and it should be $want"
	return 1
}

wait_for "$good" succeeded
wait_for "$bad" dead

# The counters must have moved. A metric that reads zero for ever is the way
# this project's measurements were wrong before, so the check is on the number
# and not on the name being present.
for name in quorra_jobs_created_total quorra_jobs_succeeded_total quorra_jobs_dead_total; do
	value=$(counter "$name")
	if [ -z "$value" ]; then
		say "$name is not published"
		exit 1
	fi
	if [ "${value%.*}" -lt 1 ]; then
		say "$name is $value, and it should have moved"
		exit 1
	fi
	say "$name is $value"
done

say "the stack works"
