//go:build unix

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/coxlong/mnevo/internal/domain"
)

func checkPrivateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || int(st.Uid) != os.Geteuid() || info.Mode().Perm()&0077 != 0 {
		return errors.New("home directory must be a private 0700 directory owned by you")
	}
	return nil
}

func readAdminKey(dir string) (string, error) {
	if err := checkPrivateDir(dir); err != nil {
		return "", err
	}
	keyPath := filepath.Join(dir, "admin.key")
	info, err := os.Lstat(keyPath)
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || int(st.Uid) != os.Geteuid() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("admin.key must be a private 0600 file owned by you")
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(string(key))
	if len(trimmed) != 64 {
		return "", errors.New("invalid admin.key")
	}
	return trimmed, nil
}

func ensureStateDirAndKey(dir string) (*os.File, string, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, "", err
	}
	if err := checkPrivateDir(dir); err != nil {
		return nil, "", err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, "", err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, "", errors.New("another mnevo service holds this home directory")
	}
	key, err := readAdminKey(dir)
	if err == nil {
		return lock, key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		_ = lock.Close()
		return nil, "", err
	}
	key = domain.Token()
	if err := os.WriteFile(filepath.Join(dir, "admin.key"), []byte(key), 0600); err != nil {
		_ = lock.Close()
		return nil, "", err
	}
	return lock, key, nil
}
