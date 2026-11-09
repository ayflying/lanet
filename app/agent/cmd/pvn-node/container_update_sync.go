package main

import (
	"os"
	"path/filepath"
	"runtime"
)

// 候选程序、签名与目录先稳定落盘，最后发布指针；防止断电后只剩半个运行槽。
func containerSyncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func containerSyncDir(path string) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func containerWriteDurable(path string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".publish-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return containerSyncDir(filepath.Dir(path))
}
