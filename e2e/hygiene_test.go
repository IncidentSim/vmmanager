package e2e

import (
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// Read only the guest account's authorized_keys via authenticated SSH. Output
// stays in memory and never reaches logs/artifacts.
func authorizedKeys(s *suite, g *guest) string {
	s.t.Helper()
	out, e := g.exec(s.ctx, `cat "$HOME/.ssh/authorized_keys"`)
	if e != nil {
		s.t.Fatal("cannot inspect guest authorized_keys")
	}
	return out
}

func managedEntries(data, id string) (owner, terminal []string) {
	for _, line := range strings.Split(data, "\n") {
		key, comment, _, _, e := ssh.ParseAuthorizedKey([]byte(line))
		if e != nil {
			continue
		}
		canonical := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		if comment == "vibe-fc-managed:owner:"+id {
			owner = append(owner, canonical)
		}
		if comment == "vibe-fc-managed:terminal:"+id {
			terminal = append(terminal, canonical)
		}
	}
	return
}

func TestCommitCredentialHygiene(t *testing.T) {
	s := newSuite(t)
	s.preflight()
	src := s.createActive("hsrc", s.cfg.BaseImageID)
	g := s.guestFor(src.VMID)
	marker := s.writeMarker(g, "hygiene")
	// Prepare installs both owner and dedicated terminal credentials. Inspect
	// those real guest entries directly: publication hygiene must not depend on
	// a browser-terminal backend being configured (the disposable API has none).
	// The assertions below still require and reject reuse of BOTH identities.
	original := authorizedKeys(s, g)
	owner, terminal := managedEntries(original, src.VMID)
	if len(owner) != 1 || len(terminal) != 1 || owner[0] == terminal[0] {
		t.Fatal("cannot identify distinct original owner and terminal entries; hygiene NOT proven")
	}
	_, legitimate := makeIdentity(t)
	// Plain ed25519 output has no shell metacharacters; controlled test key only.
	if _, e := g.exec(s.ctx, `printf '\n%s\n' '`+legitimate+` e2e-legitimate' >> "$HOME/.ssh/authorized_keys" && sync`); e != nil {
		t.Fatal("cannot install legitimate guest change")
	}
	s.stop(src.VMID)
	image := s.commit("himg", src.VMID)
	s.mustWait("hygiene commit", image.OperationID)
	s.deleteVM("hsrc", src.VMID)
	d := s.createActive("hder", image.ImageID)
	dg := s.guestFor(d.VMID)
	s.readMarker(dg, "hygiene", marker)
	after := authorizedKeys(s, dg)
	if strings.Contains(after, owner[0]) || strings.Contains(after, terminal[0]) || strings.Contains(after, "vibe-fc-managed:owner:"+src.VMID) || strings.Contains(after, "vibe-fc-managed:terminal:"+src.VMID) {
		t.Fatal("source ephemeral owner/terminal credentials survived publication")
	}
	if !strings.Contains(after, legitimate+" e2e-legitimate") {
		t.Fatal("legitimate guest authorized key was removed")
	}
	// Preserve every non-managed original line, including comments/image content.
	for _, line := range strings.Split(original, "\n") {
		if line != "" && !strings.Contains(line, "vibe-fc-managed:owner:"+src.VMID) && !strings.Contains(line, "vibe-fc-managed:terminal:"+src.VMID) && !strings.Contains(after, line) {
			t.Fatal("legitimate original authorized_keys content was lost")
		}
	}
	own2, term2 := managedEntries(after, d.VMID)
	if len(own2) != 1 || len(term2) != 1 || own2[0] == owner[0] || term2[0] == terminal[0] {
		t.Fatal("derivative credentials are not independently provisioned")
	}
	// Negatively prove original generated owner identity cannot log into derivative.
	old := *dg
	old.key = s.keys[src.VMID]
	if _, e := old.exec(s.ctx, "true"); e == nil {
		t.Fatal("source generated private key still logs into derivative")
	}
}
