// vmctl is a thin command-line client for the vmlib facade API over mTLS. It
// needs no KVM or host access. Output is JSON except `ssh-info -format ssh|ansible`.
//
//	vmctl -addr HOST:PORT -server-name NAME -ca CA -cert CERT -key KEY COMMAND ...
//	  create -image IMG -profile P [-id ID] [-ssh-public-key-file F] [-key-ref LABEL]
//	  get ID | list | images | stop ID | delete ID | commit ID NEW_IMAGE
//	  ssh-info [-format json|ssh|ansible] [-known-hosts FILE] [-key FILE] ID
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/remote"
)

func main() {
	addr := flag.String("addr", "", "facade address host:port")
	serverName := flag.String("server-name", "", "server certificate hostname to verify (required)")
	ca := flag.String("ca", "", "CA PEM that signed the server certificate")
	cert := flag.String("cert", "", "client certificate PEM")
	key := flag.String("key", "", "client private key PEM")
	timeout := flag.Duration("timeout", 10*time.Minute, "per-command deadline (bounds the call, never a VM)")
	flag.Parse()
	cfg, err := remote.ClientTLSFromFiles(*ca, *cert, *key, *serverName)
	if err == nil && *addr == "" {
		err = errors.New("-addr is required")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(2)
	}
	client, err := remote.DialFacade(*addr, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
	defer client.Close()
	// The CLI owns signals; the library does not.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	if err := execute(ctx, client, flag.Args(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
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

func execute(ctx context.Context, l vmlib.Lifecycle, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("command required: create|get|list|images|stop|commit|delete|ssh-info")
	}
	cmd, rest := args[0], args[1:]
	one := func(fs *flag.FlagSet, name string) (string, error) {
		if fs.NArg() != 1 {
			return "", fmt.Errorf("%s: exactly one %s argument required", cmd, name)
		}
		return fs.Arg(0), nil
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	switch cmd {
	case "create":
		id := fs.String("id", "", "")
		image := fs.String("image", "", "")
		profile := fs.String("profile", "", "")
		pubFile := fs.String("ssh-public-key-file", "", "")
		keyRef := fs.String("key-ref", "", "")
		if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || *image == "" || *profile == "" {
			return errors.New("usage: create -image IMG -profile P [-id ID] [-ssh-public-key-file F] [-key-ref LABEL]")
		}
		pub := ""
		if *pubFile != "" {
			b, err := os.ReadFile(*pubFile)
			if err != nil {
				return err
			}
			pub = strings.TrimSpace(string(b))
		}
		vm, err := l.Create(ctx, vmlib.CreateRequest{ID: *id, ImageID: *image, Profile: *profile, SSHPublicKey: pub, SSHKeyReference: *keyRef})
		if err != nil {
			return err
		}
		return printJSON(out, vm)
	case "get", "stop", "delete":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		id, err := one(fs, "VM id")
		if err != nil {
			return err
		}
		switch cmd {
		case "get":
			vm, err := l.Get(ctx, id)
			if err != nil {
				return err
			}
			return printJSON(out, vm)
		case "stop":
			vm, err := l.Stop(ctx, id)
			if err != nil {
				return err
			}
			return printJSON(out, vm)
		default:
			res, err := l.Delete(ctx, id)
			if err != nil {
				_ = printJSON(out, res) // retained disks are reported explicitly
				return err
			}
			return printJSON(out, res)
		}
	case "list":
		vms, err := l.List(ctx)
		if err != nil {
			return err
		}
		return printJSON(out, vms)
	case "images":
		imgs, err := l.ListImages(ctx)
		if err != nil {
			return err
		}
		return printJSON(out, imgs)
	case "commit":
		if err := fs.Parse(rest); err != nil || fs.NArg() != 2 {
			return errors.New("usage: commit VM_ID NEW_IMAGE_ID")
		}
		img, err := l.Commit(ctx, fs.Arg(0), fs.Arg(1))
		if err != nil {
			return err
		}
		return printJSON(out, img)
	case "ssh-info":
		format := fs.String("format", "json", "json|ssh|ansible")
		known := fs.String("known-hosts", "", "per-VM known_hosts file (client-local)")
		keyFile := fs.String("key", "", "client-local private key path (client-local)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		id, err := one(fs, "VM id")
		if err != nil {
			return err
		}
		info, err := l.SSHInfo(ctx, id)
		if err != nil {
			return err
		}
		switch *format {
		case "json":
			return printJSON(out, info)
		case "ssh":
			var q []string
			for _, a := range info.SSHArgs(*known, *keyFile) {
				q = append(q, shellQuote(a))
			}
			_, err := fmt.Fprintln(out, strings.Join(q, " "))
			return err
		case "ansible":
			vars := info.AnsibleVars(*keyFile)
			var parts []string
			for _, k := range []string{"ansible_host", "ansible_port", "ansible_user", "ansible_ssh_private_key_file", "ansible_ssh_common_args"} {
				if v, ok := vars[k]; ok {
					parts = append(parts, fmt.Sprintf("%s=%s", k, shellQuote(v)))
				}
			}
			_, err := fmt.Fprintf(out, "%s %s\n", id, strings.Join(parts, " "))
			return err
		}
		return fmt.Errorf("unknown format %q", *format)
	}
	return fmt.Errorf("unknown command %q", cmd)
}
