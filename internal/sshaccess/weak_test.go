package sshaccess

import (
	"crypto/rand"
	"crypto/rsa"
	"strings"

	"golang.org/x/crypto/ssh"
)

var weakRSA1024 = func() string {
	k, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		panic(err)
	}
	p, _ := ssh.NewPublicKey(&k.PublicKey)
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(p)))
}()
