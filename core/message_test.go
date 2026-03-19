package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveFilesToDisk_SanitizesFileName(t *testing.T) {
	workDir := t.TempDir()

	paths := SaveFilesToDisk(workDir, []FileAttachment{{
		FileName: "../../secret.txt",
		Data:     []byte("ok"),
	}})

	if len(paths) != 1 {
		t.Fatalf("paths len = %d, want 1", len(paths))
	}
	if got := filepath.Base(paths[0]); got != "secret.txt" {
		t.Fatalf("saved filename = %q, want %q", got, "secret.txt")
	}
	if !filepath.IsAbs(paths[0]) {
		t.Fatalf("saved path = %q, want absolute path", paths[0])
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(data) != "ok" {
		t.Fatalf("saved data = %q, want %q", string(data), "ok")
	}
}

func TestSaveFilesToDisk_SkipsOversizedFiles(t *testing.T) {
	workDir := t.TempDir()
	oversized := make([]byte, maxSavedAttachmentSize+1)

	paths := SaveFilesToDisk(workDir, []FileAttachment{{
		FileName: "big.bin",
		Data:     oversized,
	}})

	if len(paths) != 0 {
		t.Fatalf("paths len = %d, want 0", len(paths))
	}
}
