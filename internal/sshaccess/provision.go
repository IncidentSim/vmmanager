package sshaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ErrUnsupportedImage means the disk does not satisfy the documented
// provisioning format (see docs/ssh-access.md). Discard a failed working copy:
// a tool failure can occur after earlier mutations.
var ErrUnsupportedImage = errors.New("sshaccess: image does not satisfy the SSH provisioning format")

const (
	// markerPrefix labels entries; deletion requires an exact trusted receipt.
	markerPrefix = "vibe-fc-managed:"

	roleOwner    = "owner"
	roleTerminal = "terminal"
)

var (
	homePattern = regexp.MustCompile(`^/[A-Za-z0-9._][A-Za-z0-9._/-]{0,200}$`)
	userPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	rolePattern = regexp.MustCompile(`^[a-z]+$`)
	statPattern = regexp.MustCompile(`Type:\s+(\w+)\s+Mode:\s+(\d+)`)
	idsPattern  = regexp.MustCompile(`User:\s+(\d+)\s+Group:\s+(\d+)`)
)

// ManagedKey is one authorized_keys entry to install.
type ManagedKey struct{ Role, Public string }

// Provisioner installs per-VM keys into an UNMOUNTED ext4 disk copy using
// e2fsprogs debugfs, and scrubs them again before a disk is published. It never
// mounts, needs no privilege beyond write access to the image file, and operates
// only on the path it is handed.
type Provisioner struct {
	Store   *Store
	User    string // guest account that receives the keys (the backend's SSH user)
	Debugfs string // default "debugfs"
}

// Install resolves (creating once) the VM's credentials and installs the owner
// and terminal public keys into disk. suppliedPublicKey == "" selects the stable
// generated owner key. It is idempotent per disk.
func (p *Provisioner) Install(ctx context.Context, vmID, disk, suppliedPublicKey string) error {
	if p.Store == nil {
		return errors.New("sshaccess: provisioner has no credential store")
	}
	pub, err := p.Store.Ensure(vmID, suppliedPublicKey)
	if err != nil {
		return err
	}
	return p.install(ctx, vmID, disk, []ManagedKey{{roleOwner, pub.OwnerPublic}, {roleTerminal, pub.TerminalPublic}})
}

type debugfs struct{ bin string }

func d(p *Provisioner) debugfs {
	if p.Debugfs != "" {
		return debugfs{p.Debugfs}
	}
	return debugfs{"debugfs"}
}

func (g debugfs) run(ctx context.Context, disk string, write bool, cmd string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	args := []string{"-R", cmd, disk}
	if write {
		args = append([]string{"-w"}, args...)
	}
	c := exec.CommandContext(ctx, g.bin, args...)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if err := c.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("%w: debugfs failed", ErrUnsupportedImage)
	}
	s := out.String()
	if i := strings.Index(s, "\n"); i >= 0 && strings.HasPrefix(s, "debugfs ") {
		s = s[i+1:] // version banner
	}
	if strings.Contains(s, "while trying to open") || strings.Contains(s, "while opening") ||
		strings.Contains(s, "Bad magic") || strings.Contains(s, "unsupported feature") {
		return "", fmt.Errorf("%w: not a usable ext2/3/4 filesystem", ErrUnsupportedImage)
	}
	return s, nil
}

type inode struct {
	exists   bool
	typ      string
	mode     uint32
	uid, gid uint32
}

func (g debugfs) stat(ctx context.Context, disk, path string) (inode, error) {
	out, err := g.run(ctx, disk, false, "stat "+path)
	if err != nil {
		return inode{}, err
	}
	if strings.Contains(out, "File not found") {
		return inode{}, nil
	}
	m, ids := statPattern.FindStringSubmatch(out), idsPattern.FindStringSubmatch(out)
	if m == nil || ids == nil {
		return inode{}, fmt.Errorf("%w: cannot read inode", ErrUnsupportedImage)
	}
	mode, _ := strconv.ParseUint(m[2], 8, 32)
	uid, _ := strconv.ParseUint(ids[1], 10, 32)
	gid, _ := strconv.ParseUint(ids[2], 10, 32)
	return inode{true, m[1], uint32(mode), uint32(uid), uint32(gid)}, nil
}

// readFile returns the guest file's bytes. The caller has already checked it is
// a regular file.
func (g debugfs) readFile(ctx context.Context, disk, path string) ([]byte, error) {
	tmp, err := os.MkdirTemp("", "sshaccess-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	dst := filepath.Join(tmp, "f")
	out, err := g.run(ctx, disk, false, "dump "+path+" "+dst)
	if err != nil {
		return nil, err
	}
	data, rerr := os.ReadFile(dst)
	if rerr != nil {
		return nil, fmt.Errorf("%w: cannot read %s: %s", ErrUnsupportedImage, path, strings.TrimSpace(out))
	}
	return data, nil
}

// account resolves the user's uid/gid/home from the guest's /etc/passwd.
func (g debugfs) account(ctx context.Context, disk, user string) (uid, gid uint32, home string, err error) {
	if !userPattern.MatchString(user) {
		return 0, 0, "", fmt.Errorf("%w: unsafe user name", ErrUnsupportedImage)
	}
	if err := g.realAncestors(ctx, disk, "/etc/passwd"); err != nil {
		return 0, 0, "", err
	}
	ino, err := g.stat(ctx, disk, "/etc/passwd")
	if err != nil {
		return 0, 0, "", err
	}
	if !ino.exists || ino.typ != "regular" {
		return 0, 0, "", fmt.Errorf("%w: /etc/passwd is not a regular file", ErrUnsupportedImage)
	}
	data, err := g.readFile(ctx, disk, "/etc/passwd")
	if err != nil {
		return 0, 0, "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 6 || f[0] != user {
			continue
		}
		u, e1 := strconv.ParseUint(f[2], 10, 32)
		gg, e2 := strconv.ParseUint(f[3], 10, 32)
		if e1 != nil || e2 != nil || !homePattern.MatchString(f[5]) || strings.Contains(f[5], "..") || strings.Contains(f[5], "//") || strings.HasSuffix(f[5], "/") {
			return 0, 0, "", fmt.Errorf("%w: unusable passwd entry for %s", ErrUnsupportedImage, user)
		}
		return uint32(u), uint32(gg), f[5], nil
	}
	return 0, 0, "", fmt.Errorf("%w: user %s not in /etc/passwd", ErrUnsupportedImage, user)
}

// putFile (re)writes path in the guest with the given content, mode and owner,
// then reads it back to prove the write.
func (g debugfs) putFile(ctx context.Context, disk, path string, content []byte, existed bool, mode, uid, gid uint32) error {
	tmp, err := os.MkdirTemp("", "sshaccess-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	src := filepath.Join(tmp, "f")
	if err := os.WriteFile(src, content, 0o600); err != nil {
		return err
	}
	if existed {
		if _, err := g.run(ctx, disk, true, "rm "+path); err != nil {
			return err
		}
	}
	if _, err := g.run(ctx, disk, true, "write "+src+" "+path); err != nil {
		return err
	}
	for _, c := range []string{
		fmt.Sprintf("sif %s mode 0%o", path, 0o100000|mode&0o7777),
		fmt.Sprintf("sif %s uid %d", path, uid),
		fmt.Sprintf("sif %s gid %d", path, gid),
	} {
		if _, err := g.run(ctx, disk, true, c); err != nil {
			return err
		}
	}
	ino, err := g.stat(ctx, disk, path)
	if err != nil {
		return err
	}
	if !ino.exists || ino.typ != "regular" || ino.mode&0o7777 != mode&0o7777 || ino.uid != uid || ino.gid != gid {
		return fmt.Errorf("%w: write to %s not confirmed", ErrUnsupportedImage, path)
	}
	back, err := g.readFile(ctx, disk, path)
	if err != nil || !bytes.Equal(back, content) {
		return fmt.Errorf("%w: readback of %s differs", ErrUnsupportedImage, path)
	}
	return nil
}

func (p *Provisioner) install(ctx context.Context, vmID, disk string, keys []ManagedKey) error {
	g, user := d(p), p.User
	for _, k := range keys {
		if k.Role == "" || !rolePattern.MatchString(k.Role) {
			return fmt.Errorf("%w: bad role", ErrInvalidKey)
		}
		if _, err := ParsePublicKey(k.Public); err != nil {
			return err
		}
	}
	uid, gid, home, err := g.account(ctx, disk, user)
	if err != nil {
		return err
	}
	if err := g.realAncestors(ctx, disk, home+"/.ssh/authorized_keys"); err != nil {
		return err
	}
	hi, err := g.stat(ctx, disk, home)
	if err != nil {
		return err
	}
	if !hi.exists || hi.typ != "directory" {
		return fmt.Errorf("%w: home directory %s missing or not a directory", ErrUnsupportedImage, home)
	}
	sshDir, akPath := home+"/.ssh", home+"/.ssh/authorized_keys"
	di, err := g.stat(ctx, disk, sshDir)
	if err != nil {
		return err
	}
	switch {
	case !di.exists:
		if _, err := g.run(ctx, disk, true, "mkdir "+sshDir); err != nil {
			return err
		}
		for _, c := range []string{"sif " + sshDir + " mode 040700", fmt.Sprintf("sif %s uid %d", sshDir, uid), fmt.Sprintf("sif %s gid %d", sshDir, gid)} {
			if _, err := g.run(ctx, disk, true, c); err != nil {
				return err
			}
		}
	case di.typ != "directory":
		return fmt.Errorf("%w: %s is not a real directory", ErrUnsupportedImage, sshDir)
	case di.mode&0o022 != 0: // sshd StrictModes would refuse group/world-writable
		if _, err := g.run(ctx, disk, true, fmt.Sprintf("sif %s mode 0%o", sshDir, 0o040000|di.mode&0o7777&^0o022)); err != nil {
			return err
		}
	}
	if di2, err := g.stat(ctx, disk, sshDir); err != nil || !di2.exists || di2.typ != "directory" {
		return fmt.Errorf("%w: cannot prepare %s", ErrUnsupportedImage, sshDir)
	}
	fi, err := g.stat(ctx, disk, akPath)
	if err != nil {
		return err
	}
	var existing string
	switch {
	case !fi.exists:
	case fi.typ != "regular":
		return fmt.Errorf("%w: %s is not a regular file", ErrUnsupportedImage, akPath)
	default:
		data, err := g.readFile(ctx, disk, akPath)
		if err != nil {
			return err
		}
		existing = string(data)
	}
	r, err := p.Store.load(vmID)
	if err != nil {
		return err
	}
	receipt := r.Installation
	if receipt == nil {
		receipt = &Receipt{VMID: vmID, User: user, Path: akPath, Created: !fi.exists, Separator: existing != "" && !strings.HasSuffix(existing, "\n"), OriginalLength: len(existing), OriginalPrefix: existing}
		for _, k := range keys {
			canon, _ := ParsePublicKey(k.Public)
			line := canon + " " + markerPrefix + k.Role + ":" + vmID + "\n"
			preexisting := false
			for _, old := range strings.SplitAfter(existing, "\n") {
				if old == line {
					preexisting = true
					break
				}
			}
			if !preexisting {
				receipt.Lines = append(receipt.Lines, line)
			}
		}
		if err := p.Store.saveReceipt(vmID, receipt); err != nil {
			return err
		}
	} else if receipt.Path != akPath || receipt.User != user {
		return ErrConflict
	}
	// Refuse an unrelated discovery file rather than overwriting image content.
	mi, err := g.stat(ctx, disk, receiptPath)
	if err != nil {
		return err
	}
	marker, _ := json.Marshal(vmID)
	if mi.exists {
		if mi.typ != "regular" {
			return ErrUnsupportedImage
		}
		old, err := g.readFile(ctx, disk, receiptPath)
		if err != nil {
			return err
		}
		if string(old) != string(marker) {
			return ErrConflict
		}
	} else if err := g.putFile(ctx, disk, receiptPath, marker, false, 0600, 0, 0); err != nil {
		return err
	}
	// Exact line ownership only; never interpret a generic comment prefix.
	rest := removeOwned(existing, receipt)
	var b strings.Builder
	b.WriteString(rest)
	if len(rest) > 0 && !strings.HasSuffix(rest, "\n") {
		b.WriteByte('\n')
	}
	for _, line := range receipt.Lines {
		b.WriteString(line)
	}
	return g.putFile(ctx, disk, akPath, []byte(b.String()), fi.exists, 0600, uid, gid)
}

func (p *Provisioner) scrubReceipt(ctx context.Context, disk string, r *Receipt) error {
	g := d(p)
	_, _, home, err := g.account(ctx, disk, r.User)
	if err != nil {
		return err
	}
	if home+"/.ssh/authorized_keys" != r.Path {
		return ErrUnsupportedImage
	}
	if err := g.realAncestors(ctx, disk, r.Path); err != nil {
		return err
	}
	fi, err := g.stat(ctx, disk, r.Path)
	if err != nil {
		return err
	}
	if fi.exists {
		if fi.typ != "regular" {
			return ErrUnsupportedImage
		}
		data, err := g.readFile(ctx, disk, r.Path)
		if err != nil {
			return err
		}
		rest := removeOwned(string(data), r)
		if rest != string(data) {
			if r.Created && rest == "" {
				if _, err = g.run(ctx, disk, true, "rm "+r.Path); err != nil {
					return err
				}
				ino, err := g.stat(ctx, disk, r.Path)
				if err != nil || ino.exists {
					return ErrUnsupportedImage
				}
			} else if err := g.putFile(ctx, disk, r.Path, []byte(rest), true, fi.mode, fi.uid, fi.gid); err != nil {
				return err
			}
		}
	}
	mi, err := g.stat(ctx, disk, receiptPath)
	if err != nil {
		return err
	}
	if mi.exists {
		if mi.typ != "regular" {
			return ErrUnsupportedImage
		}
		data, err := g.readFile(ctx, disk, receiptPath)
		if err != nil {
			return err
		}
		expected, _ := json.Marshal(r.VMID)
		if string(data) != string(expected) {
			return ErrConflict
		}
		if _, err = g.run(ctx, disk, true, "rm "+receiptPath); err != nil {
			return err
		}
		mi, err = g.stat(ctx, disk, receiptPath)
		if err != nil || mi.exists {
			return ErrUnsupportedImage
		}
	}
	return nil
}

func removeOwned(content string, r *Receipt) string {
	var b strings.Builder
	removed := false
	for _, line := range strings.SplitAfter(content, "\n") {
		owned := false
		for _, want := range r.Lines {
			if line == want {
				owned = true
				removed = true
				break
			}
		}
		if !owned {
			b.WriteString(line)
		}
	}
	rest := b.String()
	// Restore the original unterminated bytes only when nothing follows them.
	// Otherwise the inserted newline now separates unmanaged guest content;
	// removing it could join a real key to an original comment or key line.
	if removed && r.Separator && rest == r.OriginalPrefix+"\n" {
		rest = r.OriginalPrefix
	}
	return rest
}

func (g debugfs) realAncestors(ctx context.Context, disk, path string) error {
	cur := ""
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, part := range parts[:len(parts)-1] {
		cur += "/" + part
		ino, err := g.stat(ctx, disk, cur)
		if err != nil {
			return err
		}
		if !ino.exists {
			return nil
		}
		if ino.typ != "directory" {
			return ErrUnsupportedImage
		}
	}
	return nil
}
