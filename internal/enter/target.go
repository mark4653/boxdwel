package enter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	TargetUUID  = "8173a191-47a9-497f-a254-5873a31cef0d"
	TargetName  = "vboxdistro-m1-3487a95a"
	TargetRunID = "3487a95a-a207-463b-8a4b-22769e50906f"

	OriginalOwnerSHA256 = "0113a97fa36bcd44e60f34b4753f20787c747d16ad7bd5b98b1cdcffa0d7d143"
	// fresh unicode run must not overwrite the previous prepared M2 dir
	M2DirName      = "m2-unicode"
	M2MetadataName = "m2.json"
)

// Owner is the immutable M1 ownership record that M2 binds to.
type Owner struct {
	RunID      string `json:"run_id"`
	CreatedAt  string `json:"created_at"`
	VMUUID     string `json:"vm_uuid"`
	VMName     string `json:"vm_name"`
	BaseDir    string `json:"base_folder"`
	Config     string `json:"config_file"`
	VDIPath    string `json:"vdi_path"`
	SerialPath string `json:"serial_path"`
	SourceSHA  string `json:"source_image_sha256"`
	SourceSize int64  `json:"source_image_size"`
}

// Meta is the M2 preparation record; the original owner file is never rewritten.
type Meta struct {
	Schema    string `json:"schema"`
	RunID     string `json:"run_id"`
	VMUUID    string `json:"vm_uuid"`
	VMName    string `json:"vm_name"`
	OwnerSHA  string `json:"original_owner_sha256"`
	ImageSHA  string `json:"image_sha256"`
	ImagePath string `json:"image_path"`
	VDIPath   string `json:"vdi_path"`
}

// Paths resolves the fixed run and M2 directories.
type Paths struct {
	LocalAppData string
	RunDir       string
	M2Dir        string
	Owner        string
	Meta         string
	Config       string
}

// ResolvePaths computes the fixed project paths under LOCALAPPDATA.
func ResolvePaths(localAppData string) (Paths, error) {
	var p Paths
	if localAppData == "" || !filepath.IsAbs(localAppData) {
		return p, errors.New("LOCALAPPDATA must be an absolute path")
	}
	p.LocalAppData = localAppData
	p.RunDir = filepath.Join(localAppData, "vboxdistro", "m1-test", TargetRunID)
	p.M2Dir = filepath.Join(p.RunDir, M2DirName)
	p.Owner = filepath.Join(p.RunDir, "owner.json")
	p.Meta = filepath.Join(p.M2Dir, M2MetadataName)
	p.Config = filepath.Join(p.RunDir, TargetName, TargetName+".vbox")
	return p, nil
}

func decodeOne(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("%s: expected one JSON object", path)
	}
	return nil
}

func within(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Load reads and validates the immutable owner and M2 metadata without touching the VM.
func Load(p Paths) (Owner, Meta, error) {
	var owner Owner
	var meta Meta
	if err := checkPathChain(p); err != nil {
		return owner, meta, err
	}
	if err := decodeOne(p.Owner, &owner); err != nil {
		return owner, meta, err
	}
	if owner.RunID != TargetRunID || !strings.EqualFold(owner.VMUUID, TargetUUID) || owner.VMName != TargetName {
		return owner, meta, errors.New("owner.json does not describe the approved VM")
	}
	if !strings.EqualFold(filepath.Clean(owner.BaseDir), filepath.Clean(p.RunDir)) {
		return owner, meta, errors.New("owner.json base_folder does not match the run directory")
	}
	if !strings.EqualFold(filepath.Clean(owner.Config), filepath.Clean(p.Config)) {
		return owner, meta, errors.New("owner.json config path does not match the approved config")
	}
	if owner.SourceSHA != OriginalOwnerSHA256 {
		return owner, meta, errors.New("owner.json source image hash is not the approved M1 image")
	}
	info, err := os.Stat(p.M2Dir)
	if err != nil || !info.IsDir() {
		return owner, meta, fmt.Errorf("M2 directory missing: %w", err)
	}
	if err := decodeOne(p.Meta, &meta); err != nil {
		return owner, meta, err
	}
	if meta.Schema != "m2" || meta.RunID != TargetRunID ||
		!strings.EqualFold(meta.VMUUID, TargetUUID) || meta.VMName != TargetName {
		return owner, meta, errors.New("m2.json does not describe the approved VM")
	}
	if meta.OwnerSHA != owner.SourceSHA || meta.OwnerSHA != OriginalOwnerSHA256 {
		return owner, meta, errors.New("m2.json is not bound to the original owner hash")
	}
	if !isSHA256(meta.ImageSHA) {
		return owner, meta, errors.New("m2.json image_sha256 is malformed")
	}
	for name, path := range map[string]string{"image": meta.ImagePath, "vdi": meta.VDIPath} {
		if !filepath.IsAbs(path) || !within(p.M2Dir, path) {
			return owner, meta, fmt.Errorf("m2.json %s path is outside the M2 directory", name)
		}
		if err := checkAncestors(path); err != nil {
			return owner, meta, err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return owner, meta, fmt.Errorf("m2.json %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return owner, meta, fmt.Errorf("m2.json %s is not a regular file", name)
		}
		if reparse, err := isReparse(path); err != nil {
			return owner, meta, fmt.Errorf("m2.json %s: %w", name, err)
		} else if reparse {
			return owner, meta, fmt.Errorf("m2.json %s is a reparse point", name)
		}
	}
	return owner, meta, nil
}

// checkPathChain rejects reparse points anywhere in the fixed path chain so a
// junction cannot redirect owner/config/metadata to another location.
func checkPathChain(p Paths) error {
	for _, path := range []string{p.LocalAppData, p.RunDir, p.M2Dir, p.Owner, p.Config, p.Meta} {
		if err := checkAncestors(path); err != nil {
			return err
		}
	}
	return nil
}

func checkAncestors(path string) error {
	path = filepath.Clean(path)
	for {
		reparse, err := isReparse(path)
		if err != nil {
			return err
		}
		if reparse {
			return fmt.Errorf("reparse point in path chain: %s", path)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}

func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// VerifyImage checks the prepared raw image hash before touching the VM.
func VerifyImage(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("M2 image hash mismatch: got %s want %s", got, want)
	}
	return nil
}
