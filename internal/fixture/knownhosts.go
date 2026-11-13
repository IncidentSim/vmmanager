package fixture

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// KnownHosts turns the guest's ed25519 host public key (read offline from the
// verified image, never by scanning the network) into known_hosts lines for
// the guest address of every slot.
func KnownHosts(pub []byte, prefix string, slots int) ([]byte, error) {
	key, _, _, rest, err := ssh.ParseAuthorizedKey(pub)
	if err != nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("verified image lacks exactly one valid ed25519 host public key")
	}
	if key.Type() != ssh.KeyAlgoED25519 {
		return nil, errors.New("unexpected guest host key algorithm")
	}
	var b strings.Builder
	for n := 0; n < slots; n++ {
		fmt.Fprintf(&b, "%s.%d.2 %s", prefix, n, ssh.MarshalAuthorizedKey(key))
	}
	return []byte(b.String()), nil
}
