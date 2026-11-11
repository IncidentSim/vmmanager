# Durable VM owner: real Firecracker acceptance

## Tested revision and activation

Revision `665cae2` on `feat/durable-vm-owner`, preceded by RFC `019c28a`.
The user requested live verification. The user service `firecracker-vmapi.service`
was stopped, its binary and complete closed domain/DBOS state backed up, and the
verified owner binary installed atomically. Before activation the state had no
VM records or pending/running operations. No unrelated services were changed.
The service uses the existing local backend/helper and Firecracker slot units.
The installed binary matched the independently built artifact byte-for-byte.

This is activation, not merely a fake-backend test. The new binary remains active.
Backup location and raw logs are kept in the private deployment runbook, not Git.

## Executed checks

The real HTTPS API at `https://petruekhin.tech:19453` was driven using a locally
signed short-lived user JWT (no credential values in this document). Operations
were polled to completion, with assertions on status and subsequent resource reads.

* Create returned 202, then succeeded; duplicate idempotency key attached to the
  same result, changed payload under the same key returned 409.
* Bounded SSH readiness became true, and an authenticated guest SSH command
  wrote/read a unique file marker.
* Restarting the actual API service with the VM alive preserved VM identity,
  the exact VMM process listing, and guest file marker. No guest recreation.
* Stop succeeded and yielded phase `stopped`, SSH readiness false; terminal
  admission on the stopped VM returned immediate 409.
* Two cold Commits from the same stopped VM independently succeeded and exposed
  two ready image records. Both images survived deletion of their source VM.
* Image deletions completed; subsequent image reads returned 404.
* Create followed immediately by Delete produced succeeded Create and Delete,
  followed by VM 404. This exercises Delete queued during guest creation.
* Additional image round-trip: write guest file, Commit, delete source, create a
  derivative from that image, authenticate SSH and read the exact marker.
* Deleting the image while its derivative VM existed returned 409. After deleting
  the derivative, image deletion succeeded.
* Terminal operation on an active guest succeeded and its returned capability URL
  served the real ttyd HTML through the gateway (no browser WebSocket/shell claim).
* The final domain state contained zero VM records and no pending/running
  operations. Test-created images were deleted; initial shared images untouched.
* User API service active and health OK; no warning-or-higher journal entries in
  the checked activation/test interval.

## Swagger

* `/docs` rendered actual Swagger UI in a browser with the owner-era description.
* Browser Execute of `GET /healthz` returned HTTP 200 and `{"status":"ok"}`.
* `/openapi.yaml` matched the checked-out embedded schema byte-for-byte.
* JS/CSS assets are served locally; the schema URL lives in the separate
  `swagger-initializer.js`, not inline in HTML.
* A pre-existing documentation omission was observed: create/commit name fields
  did not express the backend/controller label restrictions. It is corrected in
  a separate documentation/test change; runtime validation was not weakened.

## Evidence boundaries

These checks prove real guest boot, host-side authenticated SSH, cold image data
persistence, API restart continuity and HTTP/Swagger behavior. They do not prove
SSH reachability from a user's laptop, external-network ingress from another
host, browser terminal WebSocket interaction, power-loss behavior or every
ambiguous launcher interleaving. No forced kill of a real VMM was used. Existing
SIGKILL recovery evidence uses fake backends and remains separately labelled.

Rollback is not blindly restoring old JSON over newer operations. Preserve the
post-activation state/SQLite, stop execution, and assess handoff compatibility
before restoring a previous binary or pre-owner state. No rollback was needed.
