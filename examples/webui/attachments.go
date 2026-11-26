package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/tools"
)

const maxAttachments = 16

func inspectAttachments(paths []string) ([]toroid.Attachment, error) {
	if len(paths) > maxAttachments {
		return nil, errors.New("Choose up to 16 attachments per message.")
	}
	var items []toroid.Attachment
	seen := map[string]bool{}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return nil, errors.New("Choose an absolute path on this machine.")
		}
		path = filepath.Clean(path)
		if seen[path] {
			continue
		}
		seen[path] = true
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("Attachment unavailable: %s", path)
		}
		item := toroid.Attachment{Path: path, Kind: "file"}
		switch {
		case info.IsDir():
			item.Kind = "folder"
		case !info.Mode().IsRegular():
			return nil, errors.New("Choose a regular file or folder.")
		default:
			if mime, _ := tools.MediaType(path); strings.HasPrefix(mime, "image/") {
				if info.Size() > toroid.MaxInlineMediaBytes {
					return nil, fmt.Errorf("%s exceeds the 5 MiB image limit.", filepath.Base(path))
				}
				item.Kind = "image"
			}
		}
		items = append(items, item)
	}
	return items, nil
}

func (a *app) attachments(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Paths []string `json:"paths"`
	}
	if !decode(w, r, &input) {
		return
	}
	items, err := inspectAttachments(input.Paths)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	reply(w, items)
}

func (a *app) preview(w http.ResponseWriter, r *http.Request) {
	items, err := inspectAttachments([]string{r.URL.Query().Get("path")})
	if err != nil || items[0].Kind != "image" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(items[0].Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, toroid.MaxInlineMediaBytes+1))
	mime, _ := tools.MediaType(items[0].Path)
	if err != nil || len(data) > toroid.MaxInlineMediaBytes || http.DetectContentType(data) != mime {
		http.Error(w, "Image preview unavailable.", http.StatusUnsupportedMediaType)
		return
	}
	w.Header().Set("Content-Type", mime)
	_, _ = w.Write(data)
}
