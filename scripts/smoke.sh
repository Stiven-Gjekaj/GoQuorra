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

# counter reads one counter off the metrics page, summed over its labels.
#
# It read a bare name against a bare series, and stopped working the day a
# counter gained a label: quorra_jobs_cancelled_total is published as
# quorra_jobs_cancelled_total{caller="ops"} and the old pattern matched
# nothing, so this script reported that a counter which had moved to 2 was not
# published at all. Summing is also the right answer to "did it move", because
# the question is about the counter and not about one of its rows.
counter() {
	curl -fsS "$SERVER/metrics" | awk -v name="$1" '
		$1 == name || index($1, name "{") == 1 { total += $2; found = 1 }
		END { if (found) print total }
	'
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

# The dead letter queue is only useful if somebody can act on it.
revived=$(curl -fsS -X POST "$SERVER/v1/jobs/$bad/revive" -H "X-API-Key: $KEY" |
	sed -n 's/.*"status":"\([a-z]*\)".*/\1/p')
if [ "$revived" != "pending" ]; then
	say "reviving the dead job gave $revived"
	exit 1
fi
say "$bad was revived"
wait_for "$bad" dead

# A submission repeated under one key is one job.
key="smoke-$$"
one=$(curl -fsS -X POST "$SERVER/v1/jobs" -H "Content-Type: application/json" \
	-H "X-API-Key: $KEY" -H "Idempotency-Key: $key" \
	-d '{"type":"echo","payload":{}}' | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
two=$(curl -fsS -X POST "$SERVER/v1/jobs" -H "Content-Type: application/json" \
	-H "X-API-Key: $KEY" -H "Idempotency-Key: $key" \
	-d '{"type":"echo","payload":{}}' | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
if [ "$one" != "$two" ] || [ -z "$one" ]; then
	say "one key made two jobs: $one and $two"
	exit 1
fi
say "a repeated submission gave back $one"

# Cancelling, and the filter that finds what was cancelled.
stoppable=$(submit '{"type":"sleep","payload":{"ms":600000},"delay_seconds":3600}' |
	sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
curl -fsS -X POST "$SERVER/v1/jobs/$stoppable/cancel" -H "X-API-Key: $KEY" > /dev/null
wait_for "$stoppable" cancelled

listed=$(curl -fsS "$SERVER/v1/jobs?status=cancelled" -H "X-API-Key: $KEY")
if ! echo "$listed" | grep -q "$stoppable"; then
	say "the cancelled filter did not find $stoppable"
	exit 1
fi
if echo "$listed" | grep -q "$good"; then
	say "the cancelled filter returned a job that is not cancelled"
	exit 1
fi
say "the status filter works"

# The counters must have moved. A metric that reads zero for ever is the way
# this project's measurements were wrong before, so the check is on the number
# and not on the name being present.
for name in quorra_jobs_created_total quorra_jobs_succeeded_total quorra_jobs_dead_total \
	quorra_jobs_cancelled_total quorra_jobs_revived_total; do
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
