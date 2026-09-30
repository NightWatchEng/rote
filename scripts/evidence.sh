#!/usr/bin/env bash
# Regenerates everything under /evidence from scratch, in order:
# two model-driven discovery runs, then replays covering success, a business
# outcome, recovered conditions, a hard failure, a human handoff, and a second
# tenant. Needs the simulators running (scripts/sim.sh start) and model access
# for the discovery runs (see README). Pass --replay-only to keep the existing
# artifacts and discovery evidence and redo just the replays.
set -uo pipefail
cd "$(dirname "$0")/.."

CUA=./bin/cua
PINE="--tenant tenants/pinecrest.json"
BALANCE=artifacts/member.get_savings_balance.json
SUBACCT=artifacts/member.open_sub_account.json
CONSOLE=127.0.0.1:8090

reset() { curl -fsS -XPOST "http://127.0.0.1:${1:-8080}/_sim/reset" >/dev/null; }
fault() { curl -fsS -XPOST http://127.0.0.1:8080/_sim/fault "$@" >/dev/null; }
fresh() { rm -rf "evidence/$1"; echo; echo "=== $1"; }   # never evidence/ itself: README.md lives there
# Every run's stdout (the result as the caller receives it) is shown; the
# files under evidence/ are the redacted record.
run() { "$@" 2>/dev/null; echo "(exit $?)"; }

go build -o bin/ ./cmd/cua ./cmd/legacybank || exit 1
reset 8080; reset 8081

if [ "${1:-}" != "--replay-only" ]; then
  # The goal is given as a sentence; the value in it becomes the capability's
  # input, and the contract (id, output) comes from the flags.
  fresh 01-discovery-savings-balance
  run $CUA discover $PINE --goal "Look up member 10042 and read their current savings balance" \
    --id member.get_savings_balance --input member_id=10042 --input-pattern 'member_id=^[0-9]{1,10}$' \
    --output savings_balance:money:sensitive \
    --out $BALANCE --run-dir evidence/01-discovery-savings-balance

  # The second goal has a richer contract (an enum input, two outputs), so it
  # comes from a request file. It ends in a committing step: discovery stops
  # there and asks a human; the operator here is the CLI client of the same
  # API the console uses.
  fresh 06-discovery-open-sub-account
  $CUA discover --goal goals/open_sub_account.json $PINE --operator $CONSOLE \
    --input member_id=10042 --input "account_type=Holiday Club" --input "nickname=Vacation fund" --input opening_deposit=250.00 \
    --out $SUBACCT --run-dir evidence/06-discovery-open-sub-account 2>/dev/null &
  $CUA operator wait --timeout 5m >/dev/null &&
    $CUA operator resolve --as dana.ops --action approve --note "sandbox tenant; details on the review screen match the request"
  wait; echo
  reset 8080
fi

fresh 02-replay-success
run $CUA replay --artifact $BALANCE $PINE --input member_id=10058 --run-dir evidence/02-replay-success

fresh 03-replay-business-outcome
run $CUA replay --artifact $BALANCE $PINE --input member_id=99999 --run-dir evidence/03-replay-business-outcome

# Three injected runtime conditions in one run: an interstitial notice after
# sign-on, the session expiring as the search is submitted, and a slow member
# detail page on the second attempt.
fresh 04-replay-recovered
fault -d kind=notice -d after=1
fault -d kind=expire -d after=3
fault -d kind=slow -d after=7 -d delay_ms=3500
run $CUA replay --artifact $BALANCE $PINE --input member_id=10042 --run-dir evidence/04-replay-recovered
reset 8080

# Member 10063 exists but has no Regular Savings share. The artifact declares
# no outcome for that, so it is a hard failure with a screenshot, not a guess.
fresh 05-replay-hard-failure
run $CUA replay --artifact $BALANCE $PINE --input member_id=10063 --step-timeout 5s --run-dir evidence/05-replay-hard-failure

# A $12,000 opening deposit makes the application demand a supervisor override
# code, which automation does not have. The operator takes the live session,
# clicks the field, types the code, clicks Authorize, and hands back; then
# approves the committing step. Coordinates are the field and button centres
# from the intervention's saved observation.
fresh 07-replay-human-handoff
$CUA replay --artifact $SUBACCT $PINE --operator $CONSOLE --run-dir evidence/07-replay-human-handoff \
  --input member_id=10058 --input "account_type=Money Market" --input "nickname=Reserve" --input opening_deposit=12000.00 2>/dev/null &
op() { $CUA operator "$@" --as sam.supervisor >/dev/null; }
$CUA operator wait --timeout 2m >/dev/null
op claim
op click --x 340 --y 142
op type --text "${LEGACYBANK_OVERRIDE_CODE:-SUP-4471}"
op click --x 431 --y 142
op resolve --action resume --note "countersigned the large opening deposit"
$CUA operator wait --timeout 1m >/dev/null
op resolve --action approve --note "review screen matches the request"
wait; echo
reset 8080

# Same artifact, second tenant: same product, different wording. With the
# tenant's label overrides it runs unchanged; without them the drift is
# detected and reported instead of being clicked through.
fresh 08-replay-second-tenant
run $CUA replay --artifact $BALANCE --tenant tenants/lakeshore.json --input member_id=10042 --run-dir evidence/08-replay-second-tenant

fresh 09-replay-tenant-drift
run $CUA replay --artifact $BALANCE --tenant tenants/lakeshore-unmapped.json --input member_id=10042 --step-timeout 5s --run-dir evidence/09-replay-tenant-drift

# A business outcome on the write path: the deposit is below the product
# minimum, the application says so, and nothing is committed.
fresh 10-replay-validation-outcome
run $CUA replay --artifact $SUBACCT $PINE --operator $CONSOLE --run-dir evidence/10-replay-validation-outcome \
  --input member_id=10058 --input "account_type=Share Certificate (12 mo)" --input "nickname=CD" --input opening_deposit=100.00
reset 8080
