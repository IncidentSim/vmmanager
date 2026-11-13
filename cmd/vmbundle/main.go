// vmbundle builds the LOCAL example archive: the product executables, the
// Firecracker binary, the guest kernel, a clean prepared Ubuntu base image,
// ready configuration and a short README. Downloads and the image conversion
// happen here, never on the user's host.
//
//	vmbundle --bin bin --out DIR [--work DIR] [--image img.ext4] [--assets DIR]
//
// Without --image/--assets the pinned artifacts are downloaded (hash-verified)
// and the rootfs is converted by the shared fixture code (needs fakeroot,
// unsquashfs, mkfs.ext4 and debugfs). The archive is verified after writing.
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/petruekhin/vibe-firecracker/deployment"
	"github.com/petruekhin/vibe-firecracker/internal/fixture"
	"github.com/petruekhin/vibe-firecracker/internal/hostinstall"
)

const (
	top     = "vmmanager-example"
	imageID = "ubuntu"
	prefix  = "10.222" // matches config/pool.conf and config/vmapi.json
)

type member struct {
	name string
	mode int64
	src  string // file to read; or
	data []byte
}

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "vmbundle:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	bin := flag.String("bin", "bin", "directory with the built product executables")
	out := flag.String("out", "", "output directory for the archive")
	work := flag.String("work", "", "scratch directory (default OUT/work); holds downloads and the prepared image")
	image := flag.String("image", "", "prebuilt clean base image (skips download and conversion)")
	assets := flag.String("assets", "", "directory with firecracker and vmlinux (skips their download)")
	flag.Parse()
	if *out == "" {
		return errors.New("--out is required")
	}
	if *work == "" {
		*work = filepath.Join(*out, "work")
	}
	for _, d := range []string{*out, *work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	dl := filepath.Join(*work, "dl")
	m, err := fixture.Pinned()
	if err != nil {
		return err
	}
	var want fixture.Manifest
	for _, a := range m.Artifacts {
		switch {
		case *assets != "" && (a.Name == "firecracker" || a.Name == "vmlinux"):
		case *image != "" && a.Name == "rootfs.squashfs":
		default:
			if _, err := os.Stat(filepath.Join(dl, a.Name)); err != nil {
				want.Artifacts = append(want.Artifacts, a)
			}
		}
	}
	if len(want.Artifacts) > 0 {
		fmt.Printf("downloading %d pinned artifact(s)\n", len(want.Artifacts))
		if err := fixture.Fetch(ctx, want, dl, nil); err != nil {
			return err
		}
	}
	fc, kernel := filepath.Join(dl, "firecracker"), filepath.Join(dl, "vmlinux")
	if *assets != "" {
		fc, kernel = filepath.Join(*assets, "firecracker"), filepath.Join(*assets, "vmlinux")
	}
	img := *image
	if img == "" {
		img = filepath.Join(*work, imageID+".ext4")
		if _, err := os.Stat(img); err != nil {
			fmt.Println("preparing the Ubuntu base image")
			if err := fixture.PrepareImage(ctx, filepath.Join(dl, "rootfs.squashfs"), img, os.Stdout); err != nil {
				return err
			}
		}
	}
	if err := fixture.CheckCleanImage(ctx, img); err != nil {
		return err
	}
	ms, err := members(ctx, *bin, fc, kernel, img)
	if err != nil {
		return err
	}
	archive := filepath.Join(*out, top+".tar.gz")
	if err := writeArchive(archive, ms); err != nil {
		return err
	}
	if err := verify(ctx, archive, ms, *work); err != nil {
		return fmt.Errorf("verification of %s failed: %w", archive, err)
	}
	fmt.Println("archive:", archive)
	return nil
}

// members lists the whole archive content, sorted, with exact modes.
func members(ctx context.Context, bin, fc, kernel, img string) ([]member, error) {
	var ms []member
	for _, n := range hostinstall.BuiltNames() {
		ms = append(ms, member{name: "bin/" + n, mode: 0o755, src: filepath.Join(bin, n)})
	}
	ms = append(ms,
		member{name: "assets/firecracker", mode: 0o755, src: fc},
		member{name: "assets/vmlinux", mode: 0o644, src: kernel},
		member{name: "images/" + imageID + ".ext4", mode: 0o444, src: img},
	)
	for name, dest := range map[string]string{"README.txt": "README.txt", "vmapi.json": "config/vmapi.json", "pool.conf": "config/pool.conf"} {
		b, err := deployment.Files.ReadFile("local/" + name)
		if err != nil {
			return nil, err
		}
		ms = append(ms, member{name: dest, mode: 0o644, data: b})
	}
	// The guest host key is public and read offline from the image itself.
	pub, err := exec.CommandContext(ctx, "debugfs", "-R", "cat /etc/ssh/ssh_host_ed25519_key.pub", img).Output()
	if err != nil {
		return nil, fmt.Errorf("guest host key: %w", err)
	}
	kh, err := fixture.KnownHosts(pub, prefix, 1)
	if err != nil {
		return nil, err
	}
	ms = append(ms, member{name: "config/known_hosts", mode: 0o644, data: kh})
	for _, m := range ms {
		if m.src != "" {
			if fi, err := os.Stat(m.src); err != nil || !fi.Mode().IsRegular() {
				return nil, fmt.Errorf("missing input file %s", m.src)
			}
		}
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].name < ms[j].name })
	return ms, nil
}

func writeArchive(dest string, ms []member) error {
	tmp := dest + ".partial"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		return err
	}
	dirs := map[string]bool{}
	for _, m := range ms {
		if d := path.Dir(m.name); d != "." && !dirs[d] {
			dirs[d] = true
			if err := tw.WriteHeader(&tar.Header{Name: top + "/" + d + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
				return err
			}
		}
		size := int64(len(m.data))
		if m.src != "" {
			fi, err := os.Stat(m.src)
			if err != nil {
				return err
			}
			size = fi.Size()
		}
		if err := tw.WriteHeader(&tar.Header{Name: top + "/" + m.name, Typeflag: tar.TypeReg, Mode: m.mode, Size: size}); err != nil {
			return err
		}
		if m.src == "" {
			_, err = tw.Write(m.data)
		} else {
			var in *os.File
			if in, err = os.Open(m.src); err == nil {
				_, err = io.Copy(tw, in)
				in.Close()
			}
		}
		if err != nil {
			return err
		}
	}
	for _, c := range []io.Closer{tw, gz, f} {
		if err := c.Close(); err != nil {
			return err
		}
	}
	return os.Rename(tmp, dest)
}

// verify reads the archive back like a clean reader: member set, modes, no
// unsafe names, checksums, then extracts it into a fresh directory and runs
// the extracted vmhost with an empty environment from another directory.
func verify(ctx context.Context, archive string, ms []member, work string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	dest := filepath.Join(work, "verify")
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	want := map[string]int64{}
	for _, m := range ms {
		want[top+"/"+m.name] = m.mode
	}
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(h.Name, "/")
		if path.IsAbs(h.Name) || path.Clean(name) != name || strings.HasPrefix(name, "..") || (name != top && !strings.HasPrefix(name, top+"/")) {
			return fmt.Errorf("unsafe member name %q", h.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		if h.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("member %q is not a regular file", h.Name)
		}
		mode, ok := want[name]
		if !ok {
			return fmt.Errorf("unexpected member %q", h.Name)
		}
		if h.Mode != mode {
			return fmt.Errorf("member %q has mode %04o, want %04o", h.Name, h.Mode, mode)
		}
		seen[name] = true
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if err := os.Chmod(target, os.FileMode(mode)); err != nil {
			return err
		}
	}
	for name := range want {
		if !seen[name] {
			return fmt.Errorf("member %q is missing", name)
		}
	}
	root := filepath.Join(dest, top)
	if err := fixture.CheckCleanImage(ctx, filepath.Join(root, "images", imageID+".ext4")); err != nil {
		return err
	}
	for _, m := range ms {
		if !strings.HasSuffix(m.name, "README.txt") && !strings.HasPrefix(m.name, "config/") {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(m.name)))
		if strings.Contains(strings.ToLower(string(b)), "vibe") || strings.Contains(string(b), "/home/") {
			return fmt.Errorf("%s mentions an old name or a local path", m.name)
		}
	}
	// Source-free execution: no Go, make or source tree, empty environment, other cwd.
	cmd := exec.CommandContext(ctx, filepath.Join(root, "bin", "vmhost"))
	cmd.Dir = dest
	cmd.Env = []string{}
	out, _ := cmd.CombinedOutput() // exits 1 with the usage text
	if !strings.Contains(string(out), "usage: vmhost") {
		return fmt.Errorf("extracted vmhost did not print its usage: %q", out)
	}
	return os.RemoveAll(dest)
}
