package main

import (
	"crypto/sha256"
	"debug/elf"
	"debug/pe"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type artifact struct {
	Path   string `json:"path"`
	OS     string `json:"os"`
	Arch   string `json:"architecture"`
	SHA256 string `json:"sha256"`
}

func digest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func build() error {
	// A failed rebuild must not leave an old manifest looking current.
	if err := os.Remove("dist/manifest.json"); err != nil && !os.IsNotExist(err) {
		return err
	}
	var commit *string
	sha := "unavailable"
	if output, err := exec.Command("git", "rev-parse", "--verify", "HEAD").Output(); err == nil {
		sha = strings.TrimSpace(string(output))
		commit = &sha
	}
	manifest := struct {
		Version   string     `json:"version"`
		Commit    *string    `json:"commit"`
		Protocol  *string    `json:"protocol_version"`
		Go        string     `json:"go_version"`
		BuiltAt   string     `json:"built_at"`
		CGO       bool       `json:"cgo_enabled"`
		Artifacts []artifact `json:"artifacts"`
	}{Version: "dev", Commit: commit, Go: runtime.Version(), BuiltAt: time.Now().UTC().Format(time.RFC3339)}
	for _, target := range []struct{ os, path, pkg string }{
		{"windows", "dist/windows/boxdwel.exe", "./cmd/boxdwel"},
		{"linux", "dist/guest/boxdwel-agent", "./cmd/boxdwel-agent"},
	} {
		args := []string{"build", "-trimpath", "-buildvcs=false", "-ldflags=-X=github.com/mark4653/boxdwel/internal/buildinfo.Commit=" + sha, "-o", target.path, target.pkg}
		fmt.Printf("+ CGO_ENABLED=0 GOOS=%s GOARCH=amd64 GOAMD64=v1 go %s\n", target.os, strings.Join(args, " "))
		cmd := exec.Command("go", args...)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+target.os, "GOARCH=amd64", "GOAMD64=v1")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		if target.os == "windows" {
			f, err := pe.Open(target.path)
			if err != nil {
				return err
			}
			machine := f.Machine
			f.Close()
			if machine != pe.IMAGE_FILE_MACHINE_AMD64 {
				return fmt.Errorf("not PE/amd64: %s", target.path)
			}
		} else {
			f, err := elf.Open(target.path)
			if err != nil {
				return err
			}
			machine := f.Machine
			f.Close()
			if machine != elf.EM_X86_64 {
				return fmt.Errorf("not ELF/amd64: %s", target.path)
			}
		}
		hash, err := digest(target.path)
		if err != nil {
			return err
		}
		manifest.Artifacts = append(manifest.Artifacts, artifact{target.path, target.os, "amd64", hash})
		fmt.Printf("PASS format %s sha256=%s\n", target.path, hash)
	}
	// Keep executable test fixtures separate from the deployable artifacts.
	const nativeTest = "dist/tests/boxdwel-enter-windows.test.exe"
	if err := os.MkdirAll("dist/tests", 0755); err != nil {
		return err
	}
	args := []string{"test", "-c", "-trimpath", "-buildvcs=false", "-o", nativeTest, "./internal/enter"}
	fmt.Printf("+ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOAMD64=v1 go %s\n", strings.Join(args, " "))
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=windows", "GOARCH=amd64", "GOAMD64=v1")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	testFile, err := pe.Open(nativeTest)
	if err != nil {
		return err
	}
	machine := testFile.Machine
	testFile.Close()
	if machine != pe.IMAGE_FILE_MACHINE_AMD64 {
		return fmt.Errorf("not PE/amd64: %s", nativeTest)
	}
	testSHA, err := digest(nativeTest)
	if err != nil {
		return err
	}
	if err := os.WriteFile("dist/tests/SHA256SUMS", []byte(testSHA+"  "+nativeTest+"\n"), 0644); err != nil {
		return err
	}
	fmt.Printf("PASS test compilation/format only %s sha256=%s; Windows native execution NOT RUN\n", nativeTest, testSHA)
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile("dist/manifest.json", append(data, '\n'), 0644)
}

func main() {
	if err := build(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
