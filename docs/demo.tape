# Screen recording of the demo path. Regenerate with: vhs docs/demo.tape
# (needs `brew install vhs`, the simulators stopped, and model access for step 1).
Output docs/demo.mp4
Set Shell zsh
Set FontSize 15
Set Width 1440
Set Height 900
Set Theme "Catppuccin Mocha"
Set TypingSpeed 25ms
Set Padding 16

Hide
Type "setopt interactivecomments; clear" Enter
Show
Type "# 0. Build and start the two tenant simulators" Enter
Type "make build && ./scripts/sim.sh start" Sleep 500ms Enter
Sleep 4s

Type "clear" Enter
Type "# 1. DISCOVER: the goal in plain language; the model drives the live legacy app (no selectors, accessibility tree only)" Enter
Type "./bin/cua discover --tenant tenants/pinecrest.json --goal 'Look up member 10042 and read their current savings balance' --id member.get_savings_balance --input member_id=10042 --input-pattern 'member_id=^[0-9]{1,10}$' --output savings_balance:money:sensitive --out runs/demo-artifact.json --run-dir runs/demo-discover 2>&1 >/dev/null | scripts/decisions.sh" Sleep 500ms Enter
Sleep 60s
Type "jq '{status, turns, steps_recorded, model, backend, duration_ms, cost_usd}' runs/demo-discover/discovery.json" Sleep 500ms Enter
Sleep 6s

Type "clear" Enter
Type "# 2. REVIEW what was recorded: the contract and every step with its locators" Enter
Type "./bin/cua describe --artifact runs/demo-artifact.json" Sleep 500ms Enter
Sleep 9s

Type "clear" Enter
Type "# 3. REPLAY for a different member. No model in the loop." Enter
Type "./bin/cua replay --artifact runs/demo-artifact.json --tenant tenants/pinecrest.json --input member_id=10058 --quiet" Sleep 500ms Enter
Sleep 5s

Type "# 4. A member that does not exist: a business outcome, not a crash" Enter
Type "./bin/cua replay --artifact runs/demo-artifact.json --tenant tenants/pinecrest.json --input member_id=99999 --quiet" Sleep 500ms Enter
Sleep 5s

Type "clear" Enter
Type "# 5. Inject runtime conditions: an interstitial, a session expiry mid-flow, a slow page" Enter
Type `for f in 'kind=notice&after=1' 'kind=expire&after=3' 'kind=slow&after=7&delay_ms=3500'; do curl -s -XPOST localhost:8080/_sim/fault -d "$f" >/dev/null; done` Sleep 300ms Enter
Type "./bin/cua replay --artifact runs/demo-artifact.json --tenant tenants/pinecrest.json --input member_id=10042 --quiet | jq '{status, outputs, recoveries, attempts}'" Sleep 500ms Enter
Sleep 10s

Type "clear" Enter
Type "# 6. A hard failure with evidence: member 10063 has no savings share" Enter
Type "./bin/cua replay --artifact runs/demo-artifact.json --tenant tenants/pinecrest.json --input member_id=10063 --quiet --step-timeout 4s | jq '.failure | {kind, step, expected, screenshot}'" Sleep 500ms Enter
Sleep 9s

Type "clear" Enter
Type "# 7. HUMAN HANDOFF: a $12,000 deposit needs a supervisor override the automation does not have." Enter
Type "#    Replay runs with an operator console attached; the operator drives the SAME live session, then hands back." Enter
Type "./bin/cua replay --artifact artifacts/member.open_sub_account.json --tenant tenants/pinecrest.json --operator 127.0.0.1:8090 --input member_id=10058 --input 'account_type=Money Market' --input nickname=Reserve --input opening_deposit=12000.00 --quiet > runs/demo-handoff.json &" Sleep 500ms Enter
Sleep 1s
Type "./bin/cua operator wait | jq '.request | {kind, reason, step_id}'" Sleep 500ms Enter
Sleep 8s
Type "./bin/cua operator claim --as sam.supervisor" Sleep 300ms Enter
Sleep 1s
Type "./bin/cua operator click --as sam.supervisor --x 340 --y 142      # the Override Code field" Sleep 300ms Enter
Sleep 1s
Type "./bin/cua operator type  --as sam.supervisor --text SUP-4471        # never logged, only its length" Sleep 300ms Enter
Sleep 1s
Type "./bin/cua operator click --as sam.supervisor --x 431 --y 142      # Authorize" Sleep 300ms Enter
Sleep 1s
Type "./bin/cua operator resolve --as sam.supervisor --action resume" Sleep 300ms Enter
Sleep 3s
Type "./bin/cua operator wait | jq '.request | {kind, reason}'          # the committing step now asks for approval" Sleep 300ms Enter
Sleep 4s
Type "./bin/cua operator resolve --as sam.supervisor --action approve" Sleep 300ms Enter
Sleep 5s
Type "wait; jq '{status, outputs, handoffs: [.handoffs[] | {kind, resolution, human_actions}]}' runs/demo-handoff.json" Sleep 500ms Enter
Sleep 10s

Type "clear" Enter
Type "# 8. Same artifact, second tenant (same product, different labels) through the tenant binding" Enter
Type "./bin/cua replay --artifact runs/demo-artifact.json --tenant tenants/lakeshore.json --input member_id=10042 --quiet | jq '{status, tenant, outputs, drift}'" Sleep 500ms Enter
Sleep 6s

Type "# Done. Full tests: make test" Enter
Type "./scripts/sim.sh stop" Sleep 300ms Enter
Sleep 2s
