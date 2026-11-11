package sshaccess

import (
	"context"
	"strings"
	"testing"
)

// Exercise the combined boundary case on real ext4: an unterminated original
// comment, an inserted separator/managed keys, then a genuine guest-added key.
func TestScrubVMUnterminatedOriginalGuestChanges(t *testing.T) {
	_, guestKey, err := GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	const original = "# existing user note"
	guestLine := guestKey + " user-added\n"
	for _, tc := range []struct {
		name string
		edit func(string) string
		want string
	}{
		{
			name: "append key",
			edit: func(installed string) string { return installed + guestLine },
			want: original + "\n" + guestLine,
		},
		{
			name: "append unterminated key",
			edit: func(installed string) string { return installed + strings.TrimSuffix(guestLine, "\n") },
			want: original + "\n" + strings.TrimSuffix(guestLine, "\n"),
		},
		{
			name: "unchanged restores unterminated bytes",
			edit: func(installed string) string { return installed },
			want: original,
		},
		{
			name: "guest deletes original",
			edit: func(installed string) string { return strings.TrimPrefix(installed, original+"\n") + guestLine },
			want: guestLine,
		},
		{
			name: "guest edits original",
			edit: func(installed string) string {
				return strings.Replace(installed, original, "# guest-edited\r", 1) + guestLine
			},
			want: "# guest-edited\r\n" + guestLine,
		},
		{
			name: "guest replaces file",
			edit: func(string) string { return guestLine + "\x00# retained bytes\r\n\nunterminated" },
			want: guestLine + "\x00# retained bytes\r\n\nunterminated",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, stdPasswd)
			f.write("home/alice/.ssh/authorized_keys", original, 0600)
			img := f.image()
			p, _ := newProv(t, "alice")
			ctx := context.Background()
			const vmID = "vm-separator"
			const path = "/home/alice/.ssh/authorized_keys"
			if err := p.Install(ctx, vmID, img, ""); err != nil {
				t.Fatal(err)
			}
			installed, ok := readGuest(t, img, path)
			if !ok || !strings.HasPrefix(installed, original+"\n") {
				t.Fatalf("original/separator missing after install: %q", installed)
			}
			changed := tc.edit(installed)
			if err := d(p).putFile(ctx, img, path, []byte(changed), true, 0600, 1234, 2345); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := p.ScrubVM(ctx, vmID, img); err != nil {
					t.Fatal(err)
				}
				got, ok := readGuest(t, img, path)
				if !ok || got != tc.want {
					t.Fatalf("scrub %d must preserve unmanaged line boundaries/bytes:\nwant: %q\n got: %q", i+1, tc.want, got)
				}
			}
			fsck(t, img)
		})
	}
}
