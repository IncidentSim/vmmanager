// vmctl is a thin command-line client for the HTTP control API (the same
// /v1 API the Telegram bot and Swagger use). It needs no KVM, host access, gRPC
// or mTLS material. Output is JSON except `ssh-info -format ssh|ansible`.
//
// Connection and credentials (never on the command line):
//
//	-url URL            API base URL (env VMCTL_URL); https, or loopback http
//	-token-file FILE    private (0600) file holding ONE bearer token
//	                    (env VMCTL_TOKEN_FILE); alternatively env VMCTL_TOKEN
//	-ca-file FILE       extra root CA PEM for a private HTTPS API
//	-auth jwt|user|service
//	                    jwt (default): bot-issued RS256 user token; the API
//	                    derives the user from the token. user: static per-user
//	                    credential, needs -user-id. service: delegated static
//	                    service credential acting for -user-id.
//
// Commands:
//
//	capabilities | list | images | get ID | ssh-info [-format json|ssh|ansible] ID
//	create -image IMG [-name N] [-profile P] [-ttl-seconds N] [-key K] [-no-wait]
//	stop ID | delete ID | commit ID NEW_IMAGE_NAME | terminal ID | delete-image IMG
//	operation ID
//
// Mutations are asynchronous in the API; vmctl waits for the durable operation
// unless -no-wait is given. The token is never echoed, logged or put in errors.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/petruekhin/vibe-firecracker/controlclient"
)

const maxTokenFile = 8192

// jwtPlaceholderUser is the acting-user argument required by the SDK in jwt
// mode. The API takes the principal from the verified token and the SDK sends no
// acting-user header, so the value never reaches the server.
const jwtPlaceholderUser int64 = 1

func main() {
	if err := realMain(os.Args[1:], os.Stdout, os.Stderr, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		var u usageError
		if errors.As(err, &u) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

func realMain(args []string, out, errw io.Writer, getenv func(string) string) error {
	fs := flag.NewFlagSet("vmctl", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	url := fs.String("url", getenv("VMCTL_URL"), "")
	tokenFile := fs.String("token-file", getenv("VMCTL_TOKEN_FILE"), "")
	caFile := fs.String("ca-file", "", "")
	auth := fs.String("auth", "jwt", "")
	userID := fs.Int64("user-id", 0, "")
	timeout := fs.Duration("timeout", 10*time.Minute, "")
	if err := fs.Parse(args); err != nil {
		return usageError{"usage: vmctl [-url URL] [-token-file FILE] [-ca-file FILE] [-auth jwt|user|service] [-user-id N] COMMAND ..."}
	}
	token, err := loadToken(*tokenFile, getenv("VMCTL_TOKEN"))
	if err != nil {
		return err
	}
	if *url == "" {
		return usageError{"-url (or VMCTL_URL) is required"}
	}
	hc, err := httpClient(*caFile)
	if err != nil {
		return err
	}
	var cli *controlclient.Client
	user := *userID
	switch *auth {
	case "jwt":
		user = jwtPlaceholderUser
		cli, err = controlclient.NewPerUser(*url, func(int64) (string, error) { return token, nil }, hc)
	case "user":
		cli, err = controlclient.NewUser(*url, token, user, hc)
	case "service":
		if user <= 0 {
			return usageError{"-auth service requires -user-id"}
		}
		cli, err = controlclient.New(*url, token, hc)
	default:
		return usageError{"-auth must be jwt, user or service"}
	}
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	return execute(ctx, cli, user, fs.Args(), out, errw)
}

// loadToken reads the bearer from a private regular file or, failing that, the
// environment. The file must not be readable by group/other.
func loadToken(path, env string) (string, error) {
	var tok string
	switch {
	case path != "":
		f, err := os.Open(path)
		if err != nil {
			return "", errors.New("cannot read token file")
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 || fi.Size() > maxTokenFile {
			return "", errors.New("token file must be a private (0600) regular file")
		}
		b, err := io.ReadAll(io.LimitReader(f, maxTokenFile+1))
		if err != nil {
			return "", errors.New("cannot read token file")
		}
		tok = strings.TrimSpace(string(b))
	case env != "":
		tok = strings.TrimSpace(env)
	default:
		return "", usageError{"a token is required: -token-file FILE (or VMCTL_TOKEN_FILE / VMCTL_TOKEN)"}
	}
	if tok == "" || strings.ContainsAny(tok, " \t\r\n") {
		return "", errors.New("invalid token")
	}
	return tok, nil
}

func httpClient(caFile string) (*http.Client, error) {
	if caFile == "" {
		return &http.Client{Timeout: 30 * time.Second}, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, errors.New("cannot read CA file")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("CA file has no certificates")
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}, nil
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// shellQuote quotes for POSIX sh so output can be pasted or eval'd safely.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`!*?[]{}()<>|&;#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func newKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "vmctl-" + hex.EncodeToString(b[:]), nil
}

type bastion struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
}

// jumpSpec renders the public bastion metadata as [user@]host[:port], or "".
func jumpSpec(info controlclient.SSH) string {
	var b bastion
	if len(info.Bastion) == 0 || json.Unmarshal(info.Bastion, &b) != nil || b.Host == "" {
		return ""
	}
	jump := b.Host
	if b.User != "" {
		jump = b.User + "@" + jump
	}
	if b.Port != 0 && b.Port != 22 {
		jump += ":" + strconv.Itoa(b.Port)
	}
	return jump
}

// sshArgs builds the ssh argument vector from public SSH metadata.
func sshArgs(info controlclient.SSH, knownHosts, keyFile string) []string {
	var a []string
	if jump := jumpSpec(info); jump != "" {
		a = append(a, "-J", jump)
	}
	if keyFile != "" {
		a = append(a, "-i", keyFile)
	}
	if knownHosts != "" {
		a = append(a, "-o", "UserKnownHostsFile="+knownHosts, "-o", "StrictHostKeyChecking=accept-new")
	}
	return append(a, "-p", strconv.Itoa(info.Port), info.User+"@"+info.Host)
}

func ansibleLine(id string, info controlclient.SSH, keyFile string) string {
	parts := []string{"ansible_host=" + shellQuote(info.Host), "ansible_port=" + strconv.Itoa(info.Port), "ansible_user=" + shellQuote(info.User)}
	if keyFile != "" {
		parts = append(parts, "ansible_ssh_private_key_file="+shellQuote(keyFile))
	}
	if jump := jumpSpec(info); jump != "" {
		parts = append(parts, "ansible_ssh_common_args="+shellQuote("-o ProxyJump="+jump))
	}
	return id + " " + strings.Join(parts, " ") + "\n"
}

func execute(ctx context.Context, c *controlclient.Client, user int64, args []string, out, errw io.Writer) error {
	if len(args) == 0 {
		return usageError{"command required: capabilities|list|images|get|create|stop|delete|commit|terminal|delete-image|operation|ssh-info"}
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	one := func(name string) (string, error) {
		if err := fs.Parse(rest); err != nil || fs.NArg() != 1 {
			return "", usageError{fmt.Sprintf("usage: %s %s", cmd, name)}
		}
		return fs.Arg(0), nil
	}
	// finish waits for the durable operation (unless -no-wait) and prints both
	// the acceptance and the final operation. A failed operation exits nonzero.
	finish := func(acc controlclient.Accepted, key string, noWait bool, err error) error {
		if err != nil {
			return err
		}
		fmt.Fprintln(errw, "idempotency-key:", key)
		if noWait {
			return printJSON(out, map[string]any{"accepted": acc})
		}
		op, werr := c.Wait(ctx, user, acc.OperationID, time.Second)
		if perr := printJSON(out, map[string]any{"accepted": acc, "operation": op}); perr != nil {
			return perr
		}
		if werr != nil {
			return werr
		}
		if op.Status != "succeeded" {
			return fmt.Errorf("operation %s %s", acc.OperationID, op.Status)
		}
		return nil
	}
	keyFlag := func() (*string, *bool) {
		return fs.String("key", "", "idempotency key (default random)"), fs.Bool("no-wait", false, "")
	}
	resolveKey := func(k string) (string, error) {
		if k != "" {
			return k, nil
		}
		return newKey()
	}
	switch cmd {
	case "capabilities":
		v, err := c.Capabilities(ctx, user)
		if err != nil {
			return err
		}
		return printJSON(out, v)
	case "list":
		v, err := c.ListVMs(ctx, user)
		if err != nil {
			return err
		}
		return printJSON(out, v)
	case "images":
		v, err := c.Images(ctx, user)
		if err != nil {
			return err
		}
		return printJSON(out, v)
	case "get":
		id, err := one("VM_ID")
		if err != nil {
			return err
		}
		v, err := c.VM(ctx, user, id)
		if err != nil {
			return err
		}
		return printJSON(out, v)
	case "operation":
		id, err := one("OPERATION_ID")
		if err != nil {
			return err
		}
		v, err := c.Operation(ctx, user, id)
		if err != nil {
			return err
		}
		return printJSON(out, v)
	case "create":
		name := fs.String("name", "", "")
		image := fs.String("image", "", "")
		profile := fs.String("profile", "", "")
		ttl := fs.Int64("ttl-seconds", 0, "")
		key, noWait := keyFlag()
		if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || *image == "" || *ttl < 0 {
			return usageError{"usage: create -image IMG [-name N] [-profile P] [-ttl-seconds N] [-key K] [-no-wait]"}
		}
		req := controlclient.CreateRequest{Name: *name, ImageID: *image, Profile: *profile, TTLSeconds: *ttl}
		k, err := resolveKey(*key)
		if err != nil {
			return err
		}
		acc, err := c.Create(ctx, user, k, req)
		return finish(acc, k, *noWait, err)
	case "stop", "delete", "terminal", "delete-image":
		key, noWait := keyFlag()
		if err := fs.Parse(rest); err != nil || fs.NArg() != 1 {
			return usageError{fmt.Sprintf("usage: %s [-key K] [-no-wait] ID", cmd)}
		}
		k, err := resolveKey(*key)
		if err != nil {
			return err
		}
		id := fs.Arg(0)
		var acc controlclient.Accepted
		switch cmd {
		case "stop":
			acc, err = c.Stop(ctx, user, id, k)
		case "delete":
			acc, err = c.Delete(ctx, user, id, k)
		case "terminal":
			acc, err = c.Terminal(ctx, user, id, k)
		default:
			acc, err = c.DeleteImage(ctx, user, id, k)
		}
		return finish(acc, k, *noWait, err)
	case "commit":
		key, noWait := keyFlag()
		if err := fs.Parse(rest); err != nil || fs.NArg() != 2 {
			return usageError{"usage: commit [-key K] [-no-wait] VM_ID NEW_IMAGE_NAME"}
		}
		k, err := resolveKey(*key)
		if err != nil {
			return err
		}
		acc, err := c.Commit(ctx, user, fs.Arg(0), k, fs.Arg(1))
		return finish(acc, k, *noWait, err)
	case "ssh-info":
		format := fs.String("format", "json", "json|ssh|ansible")
		known := fs.String("known-hosts", "", "per-VM known_hosts file (client-local)")
		keyFile := fs.String("key", "", "client-local private key path (client-local)")
		if err := fs.Parse(rest); err != nil || fs.NArg() != 1 {
			return usageError{"usage: ssh-info [-format json|ssh|ansible] [-known-hosts FILE] [-key FILE] VM_ID"}
		}
		id := fs.Arg(0)
		info, err := c.SSH(ctx, user, id)
		if err != nil {
			return err
		}
		switch *format {
		case "json":
			return printJSON(out, info)
		case "ssh":
			var q []string
			for _, a := range sshArgs(info, *known, *keyFile) {
				q = append(q, shellQuote(a))
			}
			_, err := fmt.Fprintln(out, strings.Join(q, " "))
			return err
		case "ansible":
			_, err := io.WriteString(out, ansibleLine(id, info, *keyFile))
			return err
		}
		return usageError{fmt.Sprintf("unknown format %q", *format)}
	}
	return usageError{fmt.Sprintf("unknown command %q", cmd)}
}
