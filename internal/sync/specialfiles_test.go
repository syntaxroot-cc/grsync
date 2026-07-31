package sync

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestClassifySpecialFile(t *testing.T) {
	tests := []struct {
		name string
		mode fs.FileMode
		want SpecialFileType
	}{
		{"regular file", 0o644, NotSpecial},
		{"directory", fs.ModeDir | 0o755, NotSpecial},
		{"symlink", fs.ModeSymlink | 0o777, NotSpecial},
		{"named pipe", fs.ModeNamedPipe | 0o644, NamedPipe},
		{"socket", fs.ModeSocket | 0o644, Socket},
		{"char device", fs.ModeDevice | fs.ModeCharDevice | 0o644, CharDevice},
		{"block device", fs.ModeDevice | 0o644, BlockDevice},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := FileEntry{Mode: tt.mode}
			if got := ClassifySpecialFile(entry); got != tt.want {
				t.Errorf("ClassifySpecialFile(Mode=%v) = %v, want %v", tt.mode, got, tt.want)
			}
		})
	}
}

func TestApplySpecialFile_NamedPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes (FIFOs) are not supported on Windows")
	}

	dir := t.TempDir()
	destPath := filepath.Join(dir, "pipe")
	entry := FileEntry{Path: "pipe", Mode: fs.ModeNamedPipe | 0o644}

	if err := ApplySpecialFile(destPath, entry); err != nil {
		t.Fatalf("ApplySpecialFile returned error: %v", err)
	}

	info, err := os.Lstat(destPath)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if info.Mode()&fs.ModeNamedPipe == 0 {
		t.Errorf("destPath Mode = %v, want ModeNamedPipe set", info.Mode())
	}
}

func TestApplySpecialFile_UnsupportedTypesReturnErrSpecialFileUnsupported(t *testing.T) {
	tests := []struct {
		name string
		mode fs.FileMode
	}{
		{"socket", fs.ModeSocket | 0o644},
		{"char device", fs.ModeDevice | fs.ModeCharDevice | 0o644},
		{"block device", fs.ModeDevice | 0o644},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := FileEntry{Path: "special", Mode: tt.mode}
			err := ApplySpecialFile(filepath.Join(t.TempDir(), "special"), entry)
			if !errors.Is(err, ErrSpecialFileUnsupported) {
				t.Errorf("ApplySpecialFile(Mode=%v) error = %v, want it to wrap ErrSpecialFileUnsupported", tt.mode, err)
			}
		})
	}
}

func TestApplySpecialFile_NotSpecialErrors(t *testing.T) {
	entry := FileEntry{Path: "regular.txt", Mode: 0o644}
	if err := ApplySpecialFile(filepath.Join(t.TempDir(), "regular.txt"), entry); err == nil {
		t.Fatalf("ApplySpecialFile with a regular-file entry returned nil error, want an error")
	}
}
