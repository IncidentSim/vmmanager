package hostinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// target resolves an absolute target path under the staging root, refusing
// anything that could leave it: non-clean paths and symlinked ancestors or
// final components (a symlink could redirect a root-owned write elsewhere).
type target struct{ root string }

func newTarget(destDir string) (target, error) {
	if destDir == "" {
		return target{root: "/"}, nil
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return target{}, err
	}
	r, err := filepath.EvalSymlinks(destDir)
	if err != nil {
		return target{}, err
	}
	return target{root: r}, nil
}

func (t target) resolve(abs string) (string, error) {
	if !filepath.IsAbs(abs) || filepath.Clean(abs) != abs || strings.ContainsRune(abs, 0) {
		return "", fmt.Errorf("refusing unsafe destination %q", abs)
	}
	cur := t.root
	parts := strings.Split(strings.TrimPrefix(abs, "/"), "/")
	for i, p := range parts {
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			cur = filepath.Join(append([]string{cur}, parts[i+1:]...)...)
			break
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing symlink in destination path: %s", cur)
		}
		if i < len(parts)-1 && !fi.IsDir() {
			return "", fmt.Errorf("destination component is not a directory: %s", cur)
		}
	}
	rel, err := filepath.Rel(t.root, cur)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("destination %q escapes the staging root", abs)
	}
	return cur, nil
}

func (o Options) manifestAbs() string { return o.manifestPath() }

func readManifest(t target, o Options) (*Manifest, error) {
	p, err := t.resolve(o.manifestAbs())
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil || m.Version != 1 {
		return nil, fmt.Errorf("unreadable install manifest %s", p)
	}
	for _, f := range m.Files {
		if !filepath.IsAbs(f.Path) || filepath.Clean(f.Path) != f.Path {
			return nil, fmt.Errorf("manifest contains unsafe path %q", f.Path)
		}
	}
	return &m, nil
}

// Install places the planned files. It requires root unless staging into
// DestDir, performs every conflict check before the first write, and records
// ownership in the manifest last.
func Install(o Options) (*Manifest, error) {
	o = o.withDefaults()
	if o.DestDir == "" && o.Euid() != 0 {
		return nil, errors.New("installing into the live filesystem requires root (run the install with sudo, or stage with DESTDIR=...)")
	}
	plan, err := o.Plan()
	if err != nil {
		return nil, err
	}
	t, err := newTarget(o.DestDir)
	if err != nil {
		return nil, err
	}
	prev, err := readManifest(t, o)
	if err != nil {
		return nil, err
	}
	owned := map[string]string{}
	if prev != nil {
		for _, f := range prev.Files {
			owned[f.Path] = f.SHA256
		}
	}
	manifest, err := BuildManifest(o, plan)
	if err != nil {
		return nil, err
	}
	// Pass 1: resolve and detect conflicts without writing.
	var conflicts []string
	resolved := make([]string, len(plan))
	for i, e := range plan {
		p, err := t.resolve(e.Path)
		if err != nil {
			return nil, err
		}
		resolved[i] = p
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("refusing to replace non-regular destination %s", e.Path)
		}
		if _, ok := owned[e.Path]; ok {
			continue
		}
		cur, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if manifest.Files[i].SHA256 != sum(cur) && !o.Force {
			conflicts = append(conflicts, e.Path)
		}
	}
	if mp, err := t.resolve(o.manifestAbs()); err != nil {
		return nil, err
	} else if fi, err := os.Lstat(mp); err == nil && !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing non-regular manifest %s", mp)
	}
	if len(conflicts) > 0 {
		return nil, fmt.Errorf("refusing to overwrite unmanaged files (back them up and use --force to replace): %s", strings.Join(conflicts, ", "))
	}
	// Pass 2: write.
	var created []string
	if prev != nil {
		created = append(created, prev.Dirs...)
	}
	root0 := o.Euid() == 0
	for i, e := range plan {
		dirs, err := mkdirAll(t, filepath.Dir(e.Path), root0)
		if err != nil {
			return nil, err
		}
		created = append(created, dirs...)
		b, err := e.content()
		if err != nil {
			return nil, err
		}
		if err := writeAtomic(resolved[i], b, e.Mode, root0); err != nil {
			return nil, err
		}
	}
	// Files owned by a previous installation that this plan no longer ships.
	if prev != nil {
		now := map[string]bool{}
		for _, e := range plan {
			now[e.Path] = true
		}
		for _, f := range prev.Files {
			if !now[f.Path] {
				_, _ = removeOwned(t, f, false)
			}
		}
	}
	dirs, err := mkdirAll(t, filepath.Dir(o.manifestAbs()), root0)
	if err != nil {
		return nil, err
	}
	created = append(created, dirs...)
	manifest.Dirs = dedupe(created)
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	mp, _ := t.resolve(o.manifestAbs())
	if err := writeAtomic(mp, append(data, '\n'), 0o644, root0); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// mkdirAll creates missing directories (0755) and returns those it created.
func mkdirAll(t target, abs string, chownRoot bool) ([]string, error) {
	if _, err := t.resolve(abs); err != nil {
		return nil, err
	}
	var created []string
	cur := ""
	for _, part := range strings.Split(strings.TrimPrefix(abs, "/"), "/") {
		cur += "/" + part
		p, err := t.resolve(cur)
		if err != nil {
			return nil, err
		}
		fi, err := os.Lstat(p)
		if err == nil {
			if !fi.IsDir() {
				return nil, fmt.Errorf("not a directory: %s", cur)
			}
			continue
		}
		if err := os.Mkdir(p, 0o755); err != nil {
			return nil, err
		}
		if err := os.Chmod(p, 0o755); err != nil {
			return nil, err
		}
		if chownRoot {
			if err := os.Chown(p, 0, 0); err != nil {
				return nil, err
			}
		}
		created = append(created, cur)
	}
	return created, nil
}

func writeAtomic(dest string, data []byte, mode fs.FileMode, chownRoot bool) error {
	f, err := os.CreateTemp(filepath.Dir(dest), ".vmhost-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if chownRoot {
		if err := f.Chown(0, 0); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// Report is what Uninstall did.
type Report struct {
	Removed, Kept, Missing, RemovedDirs []string
}

// removeOwned removes one manifest file when it is still the file we wrote
// (matching hash) or force is set. It returns whether the file was removed.
func removeOwned(t target, f ManifestFile, force bool) (string, error) {
	p, err := t.resolve(f.Path)
	if err != nil {
		return "kept", err
	}
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "missing", nil
	}
	if err != nil {
		return "kept", err
	}
	if !fi.Mode().IsRegular() {
		return "kept", fmt.Errorf("not a regular file: %s", f.Path)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "kept", err
	}
	if sum(b) != f.SHA256 && !force {
		return "kept", nil
	}
	if err := os.Remove(p); err != nil {
		return "kept", err
	}
	return "removed", nil
}

// Uninstall removes exactly the files recorded in the manifest (skipping ones
// modified since, unless Force) and any directories the installation created
// that are now empty. User data, configuration, keys, images and state are never
// referenced and therefore never touched. It does not stop or disable units.
func Uninstall(o Options) (*Report, error) {
	o = o.withDefaults()
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if o.DestDir == "" && o.Euid() != 0 {
		return nil, errors.New("removing from the live filesystem requires root (or use DESTDIR=...)")
	}
	t, err := newTarget(o.DestDir)
	if err != nil {
		return nil, err
	}
	m, err := readManifest(t, o)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("no install manifest found: nothing owned by this installer to remove")
	}
	rep := &Report{}
	for _, f := range m.Files {
		state, err := removeOwned(t, f, o.Force)
		if err != nil {
			return rep, err
		}
		switch state {
		case "removed":
			rep.Removed = append(rep.Removed, f.Path)
		case "missing":
			rep.Missing = append(rep.Missing, f.Path)
		default:
			rep.Kept = append(rep.Kept, f.Path)
		}
	}
	if len(rep.Kept) > 0 {
		// Keep the manifest so the remaining files stay attributable.
		return rep, nil
	}
	if mp, err := t.resolve(o.manifestAbs()); err == nil {
		_ = os.Remove(mp)
	}
	dirs := append([]string(nil), m.Dirs...)
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		p, err := t.resolve(d)
		if err != nil {
			continue
		}
		if err := os.Remove(p); err == nil { // fails (kept) when not empty
			rep.RemovedDirs = append(rep.RemovedDirs, d)
		}
	}
	return rep, nil
}

// FormatManifest renders the manifest as stable "mode sha256 path" lines.
func FormatManifest(m Manifest) string {
	var b bytes.Buffer
	for _, f := range m.Files {
		b.WriteString(f.Mode + " " + f.SHA256[:12] + " " + f.Path + "\n")
	}
	return b.String()
}
