package vmbot

// TerminalCredentials supplies the server's dedicated per-VM SSH identity, not
// the owner's key. release must be idempotent. The lease lives through Wait;
// startup failures release immediately when no subprocess has been started.
type TerminalCredentials interface {
	TerminalKeyFile(vmID, runtimeDir string) (path string, release func(), err error)
}
