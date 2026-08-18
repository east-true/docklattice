//go:build linux

package discovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSecureFileFactsRejectsChangeDuringHash(t *testing.T) {
	root := t.TempDir()
	relative := "compose.yaml"
	filePath := filepath.Join(root, relative)
	if err := os.WriteFile(filePath, []byte("initial content"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := secureFileFactsWithHook(context.Background(), root, relative, func() {
		file, openErr := os.OpenFile(filePath, os.O_WRONLY|os.O_APPEND, 0)
		if openErr != nil {
			t.Fatalf("open mutation target: %v", openErr)
		}
		if _, writeErr := file.WriteString(" changed"); writeErr != nil {
			_ = file.Close()
			t.Fatalf("mutate file: %v", writeErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			t.Fatalf("close mutation target: %v", closeErr)
		}
	})
	if !HasScanErrorCode(err, CodeFileUnstable) {
		t.Fatalf("error = %v", err)
	}
}
