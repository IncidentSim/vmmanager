package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExitReceiptRequiresExactCleanProcess(t *testing.T) {
	for _, tc := range []struct {
		result, code, status string
		ok                   bool
	}{
		{"success", "exited", "0", true}, {"signal", "killed", "9", false}, {"exit-code", "exited", "1", false}, {"success", "killed", "0", false},
	} {
		t.Run(tc.result+tc.code+tc.status, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "api.sock")
			writeReceipt(t, socket, testID, tc.result, tc.code, tc.status)
			vm := &managedVM{socket: socket, id: testID, started: true}
			if err := vm.Wait(context.Background()); (err == nil) != tc.ok {
				t.Fatalf("receipt result: %v", err)
			}
		})
	}
}
func TestReplacementNeverProvesPreviousProcessExited(t *testing.T) {
	socket := apiServer(t, func(w http.ResponseWriter, r *http.Request) { instanceInfo(w, replacementID, "Not started") })
	writeReceipt(t, socket, replacementID, "success", "exited", "0")
	vm := &managedVM{socket: socket, id: testID, started: true}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := vm.Wait(ctx); err == nil {
		t.Fatal("replacement used as proof")
	}
}
func TestMalformedOrMismatchedReceiptsFailClosed(t *testing.T) {
	for _, raw := range []string{`{`, `{"invocation_id":"` + replacementID + `","result":"success","exit_code":"exited","exit_status":"0"}`, `{}`} {
		socket := filepath.Join(t.TempDir(), "api.sock")
		os.WriteFile(filepath.Join(filepath.Dir(socket), testID+".exit.json"), []byte(raw), 0600)
		vm := &managedVM{socket: socket, id: testID, started: true}
		if err := vm.Wait(context.Background()); err == nil {
			t.Fatal("accepted invalid receipt")
		}
	}
}
