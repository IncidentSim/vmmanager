package hostinstall

import (
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/petruekhin/vibe-firecracker/deployment"
)

// Entry is one regular file the installer owns.
type Entry struct {
	Path string      // absolute path on the target system (without DestDir)
	Mode fs.FileMode // exact mode
	Data []byte      // rendered content, or
	Src  string      // a source executable/asset to copy
	// RequireStatic marks executables that run inside the VMM filesystem jail.
	RequireStatic bool
}

// binary maps a built command to its installed name.
type binary struct {
	Built, Name string
	Libexec     bool
	Static      bool
}

var binaries = []binary{
	{"vmapi", "vmapi", false, false},
	{"vmbot", "vmbot", false, false},
	{"vmctl", "vmctl", false, false},
	{"vm-launcher", "fc-vm-launcher", true, true},
	{"vm-network", "fc-vm-network", true, false},
	{"vm-helper", "fc-vm-helper", true, false},
	{"vm-helper-client", "fc-vm-helper-client", true, false},
}

// BuiltNames lists the executables expected in Options.BuildDir.
func BuiltNames() []string {
	var n []string
	for _, b := range binaries {
		n = append(n, b.Built)
	}
	return n
}

type unitFile struct{ src, dest string }

var tokenLeft = regexp.MustCompile(`@[A-Z][A-Z_]*@`)

// Render substitutes the installation tokens and rejects any left over.
func (o Options) Render(src string) ([]byte, error) {
	o = o.withDefaults()
	if o.ClientGroup == "" && strings.Contains(src, "@CLIENT_GROUP@") {
		return nil, errors.New("client group is required to render units (--group)")
	}
	r := strings.NewReplacer(
		"@CLIENT_GROUP@", o.ClientGroup,
		"@LIBEXECDIR@", o.libexecdir(),
		"@BINDIR@", o.bindir(),
		"@ASSET_DIR@", o.AssetDir,
		"@STORAGE_ROOT@", o.StorageRoot,
	)
	out := r.Replace(src)
	if m := tokenLeft.FindString(out); m != "" {
		return nil, fmt.Errorf("unrendered token %s", m)
	}
	return []byte(out), nil
}

func readEmbedded(name string) (string, error) {
	b, err := deployment.Files.ReadFile(name)
	return string(b), err
}

// Plan lists every owned file with its content source and mode. It reads the
// built executables (to validate and hash them) but changes nothing.
func (o Options) Plan() ([]Entry, error) {
	o = o.withDefaults()
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if o.ClientGroup == "" {
		return nil, errors.New("client group is required (--group)")
	}
	if o.BuildDir == "" {
		return nil, errors.New("build directory is required (--build-dir)")
	}
	var plan []Entry
	for _, b := range binaries {
		dir := o.bindir()
		if b.Libexec {
			dir = o.libexecdir()
		}
		src := filepath.Join(o.BuildDir, b.Built)
		if err := checkExecutable(src, b.Static); err != nil {
			return nil, err
		}
		plan = append(plan, Entry{Path: dir + "/" + b.Name, Mode: 0o755, Src: src, RequireStatic: b.Static})
	}
	if o.Firecracker != "" {
		if err := checkExecutable(o.Firecracker, true); err != nil {
			return nil, err
		}
		plan = append(plan, Entry{Path: o.AssetDir + "/firecracker", Mode: 0o755, Src: o.Firecracker, RequireStatic: true})
	}
	if o.Kernel != "" {
		if fi, err := os.Stat(o.Kernel); err != nil || !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("kernel %q is not a regular file", o.Kernel)
		}
		plan = append(plan, Entry{Path: o.AssetDir + "/vmlinux", Mode: 0o644, Src: o.Kernel})
	}
	units := []unitFile{
		{"multivm/firecracker-vm@.service", o.unitdir() + "/firecracker-vm@.service"},
		{"multivm/firecracker-vm-network@.service", o.unitdir() + "/firecracker-vm-network@.service"},
		{"multivm/fc-vm-helper@.service", o.unitdir() + "/fc-vm-helper@.service"},
		{"multivm/fc-vm-helper.socket", o.unitdir() + "/fc-vm-helper.socket"},
		{"multivm/firecracker-vms.tmpfiles.conf", o.tmpfilesdir() + "/firecracker-vms.conf"},
		{"vmapi/vmapi.service", o.userunitdir() + "/vmapi.service"},
		{"vmbot/vmbot.service", o.userunitdir() + "/vmbot.service"},
	}
	for _, u := range units {
		text, err := readEmbedded(u.src)
		if err != nil {
			return nil, err
		}
		data, err := o.Render(text)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", u.src, err)
		}
		plan = append(plan, Entry{Path: u.dest, Mode: 0o644, Data: data})
	}
	// Examples are installed verbatim; they are documentation, never active config.
	for _, tree := range []string{"multivm", "vmapi", "vmbot"} {
		err := fs.WalkDir(deployment.Files, tree, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !(strings.Contains(p, "example") || strings.HasSuffix(p, ".slice") || path.Base(p) == "resources.conf") {
				return err
			}
			data, err := deployment.Files.ReadFile(p)
			if err != nil {
				return err
			}
			plan = append(plan, Entry{Path: o.docdir() + "/examples/" + strings.TrimPrefix(p, "multivm/"), Mode: 0o644, Data: data})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].Path < plan[j].Path })
	for i := 1; i < len(plan); i++ {
		if plan[i].Path == plan[i-1].Path {
			return nil, fmt.Errorf("duplicate destination %s", plan[i].Path)
		}
	}
	return plan, nil
}

func checkExecutable(p string, static bool) error {
	fi, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("missing built executable %s (run make build)", p)
	}
	if !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable file", p)
	}
	if static {
		f, err := elf.Open(p)
		if err != nil {
			return fmt.Errorf("%s: not an ELF executable: %w", p, err)
		}
		defer f.Close()
		for _, pr := range f.Progs {
			if pr.Type == elf.PT_INTERP {
				return fmt.Errorf("%s is dynamically linked; executables used in the filesystem jail must be static (CGO_ENABLED=0)", p)
			}
		}
	}
	return nil
}

func (e Entry) content() ([]byte, error) {
	if e.Src != "" {
		return os.ReadFile(e.Src)
	}
	return e.Data, nil
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ManifestFile is one owned file recorded in the installation manifest.
type ManifestFile struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	SHA256 string `json:"sha256"`
}

// Manifest is the ownership record used by uninstall.
type Manifest struct {
	Version int            `json:"version"`
	Prefix  string         `json:"prefix"`
	Files   []ManifestFile `json:"files"`
	// Dirs are directories this installation created (removed only if empty).
	Dirs []string `json:"dirs"`
}

// BuildManifest describes the files of a plan.
func BuildManifest(o Options, plan []Entry) (Manifest, error) {
	o = o.withDefaults()
	m := Manifest{Version: 1, Prefix: o.Prefix}
	for _, e := range plan {
		b, err := e.content()
		if err != nil {
			return m, err
		}
		m.Files = append(m.Files, ManifestFile{Path: e.Path, Mode: fmt.Sprintf("%04o", uint32(e.Mode)), SHA256: sum(b)})
	}
	return m, nil
}
