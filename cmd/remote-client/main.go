// remote-client is a standard-library-only portable mTLS build client.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"example.com/firecracker-postgres-builder/internal/remote"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("remote-client", flag.ContinueOnError)
	fs.SetOutput(out)
	endpoint := fs.String("endpoint", "", "HTTPS server endpoint, e.g. https://HOST:19443")
	ca := fs.String("tls-ca", "", "trusted CA PEM file")
	cert := fs.String("tls-cert", "", "client certificate PEM file")
	key := fs.String("tls-key", "", "client private key PEM file")
	output := fs.String("output", "", "optionally download a verified image; never overwrite")
	status := fs.String("status", "", "show one job's status instead of starting")
	cancelID := fs.String("cancel", "", "request cancellation of a job instead of starting")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 || (*status != "" && *cancelID != "") || (*output != "" && *cancelID != "") {
		return fmt.Errorf("invalid argument combination")
	}
	u, err := url.Parse(*endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("endpoint must be an HTTPS origin without credentials, path or query")
	}
	cfg, err := remote.ClientTLS(*ca, *cert, *key)
	if err != nil {
		return err
	}
	transport := &http.Transport{TLSClientConfig: cfg, Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second}
	defer transport.CloseIdleConnections()
	hc := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("redirect refused") }}
	c := &remote.Client{Endpoint: strings.TrimRight(*endpoint, "/"), HTTP: hc}
	if *cancelID != "" {
		if err := c.Cancel(ctx, *cancelID); err != nil {
			return err
		}
		fmt.Fprintln(out, "Cancellation requested; slot stays busy until child cleanup exits.")
		return nil
	}
	var job remote.Job
	if *status != "" {
		job, err = c.Get(ctx, *status)
		if err != nil {
			return err
		}
		if err := json.NewEncoder(out).Encode(job); err != nil {
			return err
		}
		if *output != "" {
			if job.State != "succeeded" {
				return fmt.Errorf("image is not ready")
			}
			return c.Download(ctx, job, *output)
		}
		return nil
	}
	job, err = c.Start(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Job %s started; interrupting this client does not cancel the server job.\n", job.ID)
	var logCursor int64
	for {
		end := job.LogOffset + int64(len(job.Log))
		if end > logCursor {
			if job.LogOffset > logCursor {
				fmt.Fprintln(out, "\n[server log tail; earlier bytes may have been truncated]")
				logCursor = job.LogOffset
			}
			fmt.Fprint(out, job.Log[logCursor-job.LogOffset:])
			logCursor = end
		}
		switch job.State {
		case "succeeded":
			fmt.Fprintf(out, "\nJob %s succeeded; image retained server-side (%d bytes, SHA256 %s).\n", job.ID, job.Size, job.SHA256)
			if *output != "" {
				if err := c.Download(ctx, job, *output); err != nil {
					return err
				}
				fmt.Fprintf(out, "Downloaded %s\n", *output)
			}
			return nil
		case "failed", "cancelled":
			return fmt.Errorf("job %s %s: %s", job.ID, job.State, job.Error)
		case "running", "cancelling":
		default:
			return fmt.Errorf("unrecognized job state %q", job.State)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("job %s remains server-side: %w", job.ID, ctx.Err())
		case <-timer.C:
		}
		job, err = c.Get(ctx, job.ID)
		if err != nil {
			return fmt.Errorf("job %s remains server-side: %w", job.ID, err)
		}
	}
}
