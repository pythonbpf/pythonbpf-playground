package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// Image is one selectable playground version: a guest rootfs with a given
// pythonbpf + pylibbpf pair, stored as <images dir>/<id>/rootfs.squashfs
// with optional metadata in <id>/meta.json.
type Image struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Default   bool   `json:"default,omitempty"`
	Order     int    `json:"order"`
	PythonBPF string `json:"pythonbpf,omitempty"`
	PyLibBPF  string `json:"pylibbpf,omitempty"`
	Built     string `json:"built,omitempty"`
}

var validImageID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// imageRootfs maps a (client-supplied) image id to its rootfs path. It is the
// only way an id becomes a path, so ids can never escape the images dir.
func imageRootfs(dir, id string) (string, error) {
	if !validImageID.MatchString(id) {
		return "", errors.New("invalid version id")
	}
	p := filepath.Join(dir, id, "rootfs.squashfs")
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return "", errors.New("unknown version")
	}
	return p, nil
}

// loadImages lists the available versions, sorted by order then id, with
// exactly one marked as the default.
func loadImages(dir string) ([]Image, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var imgs []Image
	for _, e := range entries {
		id := e.Name()
		if !e.IsDir() {
			continue
		}
		if _, err := imageRootfs(dir, id); err != nil {
			continue
		}
		img := Image{ID: id, Label: id, Order: 1000} // no metadata: sorts last
		if b, err := os.ReadFile(filepath.Join(dir, id, "meta.json")); err == nil {
			json.Unmarshal(b, &img)
			img.ID = id
			if img.Label == "" {
				img.Label = id
			}
		}
		imgs = append(imgs, img)
	}
	if len(imgs) == 0 {
		return nil, errors.New("no playground images found in " + dir)
	}
	sort.SliceStable(imgs, func(a, b int) bool {
		if imgs[a].Order != imgs[b].Order {
			return imgs[a].Order < imgs[b].Order
		}
		return imgs[a].ID < imgs[b].ID
	})
	def := -1
	for i := range imgs {
		if imgs[i].Default && def < 0 {
			def = i
		} else {
			imgs[i].Default = false
		}
	}
	if def < 0 {
		imgs[0].Default = true
	}
	return imgs, nil
}

func defaultImage(imgs []Image) string {
	for _, i := range imgs {
		if i.Default {
			return i.ID
		}
	}
	return imgs[0].ID
}
