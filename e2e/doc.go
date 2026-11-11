// Package e2e holds opt-in end-to-end tests for a REAL deployed vmapi, driven
// only through its public HTTPS API (and, for guest checks, the ssh client).
// Everything is skipped unless VMAPI_E2E=1. See docs/e2e-live.md.
package e2e
