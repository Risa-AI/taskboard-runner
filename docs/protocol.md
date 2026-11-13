# Fleet protocol

How a runner and taskboard talk. The runner makes every call; taskboard
never calls the runner, so the runner's host opens no inbound port. The
types are in [`pkg/protocol`](../pkg/protocol/protocol.go), which both sides
import, and this page describes the calls they make with them.

## Every call

- **Base path:** `/v1/runner`, on the taskboard address the runner is given
  (`TB_URL`), over HTTPS.
- **Headers:**
  - `X-Fleet-Token: <token>`: the fleet's token, shown once when the fleet is
    registered. It can be rotated and disabled in taskboard.
  - `X-Runner-Protocol: 1`: the protocol version (`protocol.ProtocolVersion`).
  - `Content-Type: application/json` on a call with a body.
- **Answers:** a call that answers with a body wraps it in taskboard's usual
  envelope, `{"success": true, "data": ...}`. A refusal is
  `{"success": false, "error": {"code": "...", "message": "..."}}`, and its
  message is written for a person.
- **Nothing secret travels.** taskboard learns secret names, never values,
  and everything the runner posts about a run is masked before it leaves the
  host.

These answers can come back from any call:

| Status | Code | Meaning | The runner |
|---|---|---|---|
| 401 | `UNAUTHORIZED` | The token is missing, unknown, rotated or disabled. | Stops, and says the token was refused. |
| 426 | `PROTOCOL_TOO_OLD` | taskboard no longer speaks this protocol version. The message names the oldest runner version it accepts. | Stops, and says to update. |
| 400, 422 | `VALIDATION_ERROR` | The body is not JSON of the call's shape (400), or breaks one of its rules (422). | Reports a bug; does not retry the same body. |
| 5xx, or no answer | | taskboard is unavailable. | Retries with back-off. |

## `PUT /v1/runner/environments`

Which secrets this fleet holds, for each environment in its environments
file. Sent when the runner starts and whenever the file changes. It replaces
what the fleet reported before.

```json
[{"name": "staging", "secrets": ["admin_password", "api_key"]}]
```

- **204**: stored.

## `POST /v1/runner/heartbeat`

Sent every 30 seconds. A fleet not seen for 2 minutes shows as offline, and
its queued runs wait for it.

```json
{"version": "v0.3.0", "slots_free": 1}
```

- **200**: `{"success": true, "data": {"runs_to_cancel": ["<run id>"]}}`. The
  runner stops each named run it holds; a cancel travels back this way
  because taskboard never calls in.

## `POST /v1/runner/claim`

Asks for one run. The body is `{}`. taskboard holds the call open for up to
25 seconds while there is nothing to run.

- **200**: `{"success": true, "data": <job>}`. The run is this fleet's from
  now on.
- **204**: nothing to run. The runner calls again at once.

The job holds no secret; the runner adds the environment's secrets from its
own file:

```json
{
  "run_id": "<run id>",
  "suite": {
    "files": [{"name": "01_auth.yml", "content": "<the file's YAML>"}],
    "seeds": {"n": {"value": "0"}},
    "fixtures": [{"name": "fixtures/proof.png", "url": "<a short-lived link>"}]
  },
  "environment": {"name": "staging", "base_url": "https://staging.example.com", "variables": {"admin_email": "qa@example.com"}},
  "timeout_s": 1200,
  "lease_s": 120
}
```

- `files` are scenario files in the suite format of
  [`pkg/apitest`](../pkg/apitest/format.go); `seeds` are its seeds.
- `fixtures` are downloaded into a directory of the run's own, which is
  deleted when the run ends.
- `timeout_s` is how long the whole run may take.
- `lease_s` is how long the run may go without a call about it. A claimed run
  that stays silent past its lease is failed: "fleet went silent".

## `POST /v1/runner/runs/:id/cases`

Each case's result, sent as it finishes; one call may carry several.
Request and response are masked and cut to 2 KB before they are sent. The
call extends the run's lease.

```json
[{"index": 3, "file": "02_me.yml", "line": 5, "name": "me", "method": "GET", "path": "/v1/me",
  "status": 500, "outcome": "fail", "duration_ms": 42, "reason": "expected status 200, got 500",
  "request": "GET /v1/me ...", "response": "{\"held\":\"[redacted]\"}"}]
```

- **204**: stored. A case is kept once per `(run_id, index)`, so sending it
  again after a lost answer changes nothing.
- **404** `NOT_FOUND`: the run is not one this fleet claimed. It answers the
  same for a run that does not exist, so a fleet learns nothing about other
  fleets' runs.
- **409** `CONFLICT`: the run has already ended (cancelled, or failed as
  silent). The runner stops the run.

## `POST /v1/runner/runs/:id/complete`

The run's last call.

```json
{"summary": {"verdict": "failed", "total": 4, "ok": 2, "skipped": 1, "failed": 1, "not_run": 0},
 "error": "secret admin_password of environment staging is empty"}
```

- `verdict` is `passed`, `passed_with_skips`, `failed` or `refused` (no case
  ran).
- `error` says why a run could not run at all, masked. It is left out when
  there is none.

Answers:

- **204**: the run is finished.
- **404** `NOT_FOUND`: as for cases.
- **409** `CONFLICT`: the run had already ended and keeps that status. The
  runner treats this as done and does not retry.

## Versions

`X-Runner-Protocol` changes only for a change that would break the other
side. A field added to a body is not one: each side ignores fields it does
not know. Renaming or removing a field, or changing what an answer means,
is a new protocol version. `pkg/protocol`'s tests pin every field name.
