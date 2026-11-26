package main

import (
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	toroid "github.com/yashbonde/toroid-kernel"
)

// The catalog comes from the server's configured provider. Keep the route on
// the value, even when a gateway model has an ID starting with "openai/".
func (a *app) catalogProvider() (string, string, string) {
	base := os.Getenv(toroid.GatewayBaseURLEnv)
	key := os.Getenv(toroid.GatewayKeyEnv)
	prefix := "llmgateway/"
	direct := strings.HasPrefix(a.base.Settings.Model, "openai/")
	if base == "" && direct {
		base, key, prefix = toroid.OpenAIBaseURL, os.Getenv(toroid.OpenAIKeyEnv), "openai/"
	}
	if override := os.Getenv("TOROID_LLM_TOKEN"); override != "" && (prefix == "openai/") == direct {
		key = override
	}
	return base, key, prefix
}

func (a *app) models(w http.ResponseWriter, r *http.Request) {
	base, key, prefix := a.catalogProvider()
	items := toroid.CachedGatewayModels(r.Context(), base, key)
	if items == nil {
		http.Error(w, "Model catalog unavailable. Check the configured provider and retry.", http.StatusBadGateway)
		return
	}
	type option struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		Images        bool   `json:"images"`
		ContextLength int    `json:"contextLength,omitempty"`
	}
	result := make([]option, 0, len(items))
	for _, m := range items {
		id := prefix + strings.TrimPrefix(m.ID, prefix)
		result = append(result, option{id, m.ID, toroid.ResolveModel(id).SupportsImage(), m.ContextLength})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	reply(w, result)
}

// The browser and kernel share this machine, but browser file inputs do not
// expose absolute paths. List directories through the authenticated local API.
func directory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("expected an absolute folder path")
	}
	path, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("expected a folder")
	}
	return path, nil
}

func (a *app) folders(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = a.workdir
	}
	path, err := directory(path)
	if err != nil {
		http.Error(w, "That folder is unavailable.", 400)
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		http.Error(w, "Could not open that folder.", 403)
		return
	}
	type folder struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	folders := []folder{}
	files := []folder{}
	includeFiles := r.URL.Query().Get("files") == "1"
	for _, entry := range entries {
		if !includeFiles && strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		child := filepath.Join(path, entry.Name())
		if includeFiles && !entry.IsDir() {
			info, err := os.Stat(child)
			if err != nil {
				continue
			}
			if info.Mode().IsRegular() {
				files = append(files, folder{entry.Name(), child})
				continue
			}
		}
		if !entry.IsDir() {
			if entry.Type()&os.ModeSymlink == 0 {
				continue
			}
			if _, err := directory(child); err != nil {
				continue
			}
		}
		folders = append(folders, folder{entry.Name(), child})
	}
	reply(w, map[string]any{"path": path, "parent": filepath.Dir(path), "folders": folders, "files": files})
}

type chatOptions struct {
	Workspace string `json:"workspace"`
	CWD       string `json:"cwd"`
	Model     string `json:"model"`
}

func (a *app) options(r *http.Request, id string) (chatOptions, error) {
	o := chatOptions{CWD: a.workdir}
	err := a.db.QueryRowContext(r.Context(), `SELECT workspace,cwd,model FROM webui_session_options WHERE trace_id=?`, id).Scan(&o.Workspace, &o.CWD, &o.Model)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return o, err
}

func (a *app) deleteAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "base" {
		http.Error(w, "The base agent is built in.", 400)
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		dbError(w, err)
		return
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(r.Context(), `DELETE FROM webui_agents WHERE id=?`, id)
	if err != nil {
		dbError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		dbError(w, sql.ErrNoRows)
		return
	}
	if _, err = tx.ExecContext(r.Context(), `UPDATE webui_chats SET agent='base' WHERE agent=?`, id); err != nil {
		dbError(w, err)
		return
	}
	if err = tx.Commit(); err != nil {
		dbError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
