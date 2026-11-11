// Package e2e holds opt-in end-to-end tests for a REAL deployed vmapi, driven
// through its public HTTPS API and real guest SSH. Explicit optional gates
// additionally allow API user-unit restart and local kernel CPU readback.
// Everything is skipped unless VMAPI_E2E=1. See docs/e2e-live.md.
package e2e
