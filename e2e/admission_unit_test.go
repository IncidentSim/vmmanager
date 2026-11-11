package e2e

import (
	"context"
	"encoding/json"
	"github.com/petruekhin/vibe-firecracker/controlclient"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Exercise the real SDK's Accepted decoding, including successes where live
// assertions expect a conflict. Cleanup must own outputs before assertions.
func TestAcceptedIntentSDKAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name              string
		intent            acceptedIntent
		vm, image         string
		wantVM, wantImage string
		wantErr           bool
	}{
		{"create base", acceptedCreate, "new-vm", "ubuntu-base", "new-vm", "", false},
		{"create preexisting source", acceptedCreate, "new-vm", "pre-img", "new-vm", "", false},
		{"unexpected changed-payload conflict success", acceptedCreate, "stray-vm", "ubuntu-base", "stray-vm", "", false},
		{"unexpected replay success", acceptedCreate, "stray-vm", "pre-img", "stray-vm", "", false},
		{"commit both IDs", acceptedCommit, "pre-vm", "new-img", "", "new-img", false},
		{"unexpected commit conflict success", acceptedCommit, "pre-vm", "stray-img", "", "stray-img", false},
		{"commit base refused", acceptedCommit, "pre-vm", "ubuntu-base", "", "", true},
		{"commit baseline refused", acceptedCommit, "pre-vm", "pre-img", "", "", true},
		{"create baseline refused", acceptedCreate, "pre-vm", "ubuntu-base", "", "", true},
		{"terminal references", acceptedReferences, "pre-vm", "pre-img", "", "", false},
		{"stop references", acceptedReferences, "pre-vm", "ubuntu-base", "", "", false},
		{"delete references", acceptedReferences, "pre-vm", "pre-img", "", "", false},
		{"delete image references", acceptedReferences, "pre-vm", "ubuntu-base", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(controlclient.Accepted{OperationID: "op", VMID: tc.vm, ImageID: tc.image})
			}))
			defer server.Close()
			c, err := controlclient.NewUser(server.URL, "fixture-token", 1, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			tr := &tracker{base: "ubuntu-base", user: 1, keySeq: newNamer().key}
			tr.setBaseline([]string{"pre-vm"}, []string{"ubuntu-base", "pre-img"})
			var a controlclient.Accepted
			switch tc.intent {
			case acceptedCreate:
				a, err = c.Create(context.Background(), 1, "reused-key", controlclient.CreateRequest{ImageID: tc.image, Name: "changed"})
			case acceptedCommit:
				a, err = c.Commit(context.Background(), 1, tc.vm, "reused-key", "output")
			default:
				switch {
				case strings.HasPrefix(tc.name, "terminal"):
					a, err = c.Terminal(context.Background(), 1, tc.vm, "key")
				case strings.HasPrefix(tc.name, "stop"):
					a, err = c.Stop(context.Background(), 1, tc.vm, "key")
				case strings.HasPrefix(tc.name, "delete image"):
					a, err = c.DeleteImage(context.Background(), 1, tc.image, "key")
				default:
					a, err = c.Delete(context.Background(), 1, tc.vm, "key")
				}
			}
			trackErr := tr.trackAccepted(tc.intent, a) // before checking SDK status
			if err != nil {
				t.Fatal(err)
			}
			if (trackErr != nil) != tc.wantErr {
				t.Fatalf("tracking: %v", trackErr)
			}
			f := &fakeAPI{vms: map[string]bool{"pre-vm": true}, images: map[string]int{"ubuntu-base": 0, "pre-img": 0}}
			if tc.wantVM != "" {
				f.vms[tc.wantVM] = true
			}
			if tc.wantImage != "" {
				f.images[tc.wantImage] = 0
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if errs := tr.cleanup(ctx, f, time.Millisecond); len(errs) > 0 {
				t.Fatal(errs)
			}
			var want []string
			if tc.wantVM != "" {
				want = append(want, "vm:"+tc.wantVM)
			}
			if tc.wantImage != "" {
				want = append(want, "img:"+tc.wantImage)
			}
			if strings.Join(f.deleted, ",") != strings.Join(want, ",") {
				t.Fatalf("deletions=%v want %v", f.deleted, want)
			}
			if !f.vms["pre-vm"] {
				t.Fatal("baseline VM deleted")
			}
			for _, id := range []string{"ubuntu-base", "pre-img"} {
				if _, ok := f.images[id]; !ok {
					t.Fatalf("protected image deleted: %s", id)
				}
				if tr.mayDeleteImage(id) == nil {
					t.Fatalf("protected image deletion permitted: %s", id)
				}
			}
		})
	}
}

func TestCreateAcceptedSourceReference(t *testing.T) {
	tr := &tracker{base: "ubuntu-base"}
	tr.setBaseline(nil, []string{"ubuntu-base", "existing-source"})
	s := &suite{t: t, tr: tr}
	s.trackAccepted(acceptedCreate, controlclient.Accepted{VMID: "new-vm", ImageID: "ubuntu-base", OperationID: "create-op"})
	if len(tr.vms) != 1 || len(tr.images) != 0 {
		t.Fatalf("create tracked references as admissions: VMs=%v images=%v", tr.vms, tr.images)
	}
}
