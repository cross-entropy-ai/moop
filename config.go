package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const defaultServer = "https://audio.xtra.science"

// Config is what `moop login` saves.
type Config struct {
	Server string `json:"server,omitempty"`
	Token  string `json:"token,omitempty"`
}

func configPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "moop", "config.json"), nil
}

func loadConfig() (Config, error) {
	var c Config
	p, err := configPath()
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, errors.New(p + ": " + err.Error())
	}
	return c, nil
}

func saveConfig(c Config) (string, error) {
	p, err := configPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return p, writeAtomic(p, append(b, '\n'), 0o600)
}

func removeConfig() (string, error) {
	p, err := configPath()
	if err != nil {
		return "", err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return p, nil
}

// resolve picks server and token: flag > environment > config file > default.
func resolve(flagServer string) (Config, error) {
	file, err := loadConfig()
	if err != nil {
		return Config{}, err
	}
	c := Config{Server: firstNonEmpty(flagServer, os.Getenv("MOOP_SERVER"), file.Server, defaultServer)}
	c.Server = strings.TrimRight(c.Server, "/")
	// A saved token belongs to the server it was saved for.
	fileToken := ""
	if strings.TrimRight(firstNonEmpty(file.Server, defaultServer), "/") == c.Server {
		fileToken = file.Token
	}
	c.Token = firstNonEmpty(os.Getenv("MOOP_TOKEN"), fileToken)
	return c, nil
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// writeAtomic writes to a temp file next to path and renames it into place,
// so readers never see a half-written file.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
