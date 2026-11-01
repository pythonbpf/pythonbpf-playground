package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestImages(t *testing.T) {
	dir := t.TempDir()
	mk := func(id, meta string) {
		os.MkdirAll(filepath.Join(dir, id), 0o755)
		os.WriteFile(filepath.Join(dir, id, "rootfs.squashfs"), []byte("x"), 0o644)
		if meta != "" {
			os.WriteFile(filepath.Join(dir, id, "meta.json"), []byte(meta), 0o644)
		}
	}
	mk("0.2.0", `{"label":"0.2.0","order":1}`)
	mk("master", `{"label":"master","default":true,"order":0}`)
	mk("nometa", "")
	os.MkdirAll(filepath.Join(dir, "empty"), 0o755) // no rootfs: skipped

	imgs, err := loadImages(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 3 || imgs[0].ID != "master" || !imgs[0].Default || imgs[1].ID != "0.2.0" || imgs[1].Default {
		t.Fatalf("unexpected catalog: %+v", imgs)
	}
	for _, bad := range []string{"../master", "master/../0.2.0", "", ".hidden", "a/b", "empty"} {
		if _, err := imageRootfs(dir, bad); err == nil {
			t.Errorf("imageRootfs(%q) should fail", bad)
		}
	}
	if p, err := imageRootfs(dir, "master"); err != nil || p != filepath.Join(dir, "master", "rootfs.squashfs") {
		t.Errorf("imageRootfs(master) = %q, %v", p, err)
	}
}
