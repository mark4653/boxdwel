package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(path, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := digest(path)
	if err != nil || got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("digest=%s error=%v", got, err)
	}
	if _, err := digest(path + ".missing"); err == nil {
		t.Fatal("missing file must fail")
	}
}
