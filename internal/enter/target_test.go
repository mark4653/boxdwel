package enter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) (Paths, Owner, Meta, string) {
	t.Helper()
	local := t.TempDir()
	p, err := ResolvePaths(local)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p.Config), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Config, []byte("<vm/>"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.M2Dir, 0755); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(p.M2Dir, "m1-boot.img")
	if err := os.WriteFile(image, []byte("raw"), 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("raw"))
	imageSHA := hex.EncodeToString(sum[:])
	vdi := filepath.Join(p.M2Dir, "m1-boot.vdi")
	if err := os.WriteFile(vdi, []byte("vdi"), 0600); err != nil {
		t.Fatal(err)
	}
	owner := Owner{RunID: TargetRunID, VMUUID: TargetUUID, VMName: TargetName,
		BaseDir: p.RunDir, Config: p.Config, SourceSHA: OriginalOwnerSHA256}
	meta := Meta{Schema: "m2", RunID: TargetRunID, VMUUID: TargetUUID, VMName: TargetName,
		OwnerSHA: OriginalOwnerSHA256, ImageSHA: imageSHA, ImagePath: image, VDIPath: vdi}
	writeJSON(t, p.Owner, owner)
	writeJSON(t, p.Meta, meta)
	return p, owner, meta, image
}

func TestLoadValid(t *testing.T) {
	p, _, meta, image := fixture(t)
	owner, got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if owner.VMUUID != TargetUUID || got.ImagePath != meta.ImagePath {
		t.Fatalf("owner=%+v meta=%+v", owner, got)
	}
	if err := VerifyImage(image, meta.ImageSHA); err != nil {
		t.Fatal(err)
	}
	if err := VerifyImage(image, OriginalOwnerSHA256); err == nil {
		t.Fatal("wrong hash accepted")
	}
}

func TestLoadRejectsTampering(t *testing.T) {
	cases := map[string]func(t *testing.T, p Paths, owner *Owner, meta *Meta){
		"owner uuid":   func(t *testing.T, p Paths, o *Owner, m *Meta) { o.VMUUID = "11111111-1111-1111-1111-111111111111" },
		"owner name":   func(t *testing.T, p Paths, o *Owner, m *Meta) { o.VMName = "other" },
		"owner base":   func(t *testing.T, p Paths, o *Owner, m *Meta) { o.BaseDir = filepath.Dir(p.RunDir) },
		"owner config": func(t *testing.T, p Paths, o *Owner, m *Meta) { o.Config = filepath.Join(p.M2Dir, "x.vbox") },
		"owner hash":   func(t *testing.T, p Paths, o *Owner, m *Meta) { o.SourceSHA = m.ImageSHA },
		"meta schema":  func(t *testing.T, p Paths, o *Owner, m *Meta) { m.Schema = "m1" },
		"meta owner hash": func(t *testing.T, p Paths, o *Owner, m *Meta) {
			m.OwnerSHA = "0000000000000000000000000000000000000000000000000000000000000000"
		},
		"meta image hash": func(t *testing.T, p Paths, o *Owner, m *Meta) { m.ImageSHA = "short" },
		"meta image path": func(t *testing.T, p Paths, o *Owner, m *Meta) { m.ImagePath = filepath.Join(p.RunDir, "x.img") },
		"meta vdi path":   func(t *testing.T, p Paths, o *Owner, m *Meta) { m.VDIPath = filepath.Join(p.RunDir, "x.vdi") },
		"meta missing file": func(t *testing.T, p Paths, o *Owner, m *Meta) {
			m.VDIPath = filepath.Join(p.M2Dir, "absent.vdi")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p, owner, meta, _ := fixture(t)
			mutate(t, p, &owner, &meta)
			writeJSON(t, p.Owner, owner)
			writeJSON(t, p.Meta, meta)
			if _, _, err := Load(p); err == nil {
				t.Fatal("tampered metadata accepted")
			}
		})
	}
}

func TestResolvePathsRejectsRelativeLocalAppData(t *testing.T) {
	if _, err := ResolvePaths("relative"); err == nil {
		t.Fatal("relative LOCALAPPDATA accepted")
	}
	if _, err := ResolvePaths(""); err == nil {
		t.Fatal("empty LOCALAPPDATA accepted")
	}
}
