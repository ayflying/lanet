package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/ayflying/pvn/pkg/selfupdate"
)

// containerStageUpdate 只发布签名且摘要匹配的不可变运行槽。
// candidate 指针最后原子发布；监督器在子进程退出后再次独立验签。
func containerStageUpdate(source string, m selfupdate.Manifest) error {
	dir := containerRuntimeDir()
	if dir == "" {
		return fmt.Errorf("容器监督器未就绪，不能安全切换程序")
	}
	if !m.Verify(selfupdate.ReleasePublicKey) || m.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		return fmt.Errorf("容器更新清单签名或平台无效")
	}
	sum, err := selfupdate.FileSHA256(source)
	if err != nil {
		return err
	}
	st, err := os.Stat(source)
	if err != nil {
		return err
	}
	if sum != m.SHA256 || st.Size() != m.Size {
		return fmt.Errorf("容器更新程序与签名清单不匹配")
	}
	if selfupdate.CompareVersions(m.Version, version) <= 0 {
		return fmt.Errorf("拒绝安装非更新版本 %s", m.Version)
	}
	// 已失败的摘要持久隔离，但不妨碍后续更高版本继续自动更新。
	if raw, err := os.ReadFile(filepath.Join(dir, "failed.json")); err == nil {
		var failed map[string]json.RawMessage
		if json.Unmarshal(raw, &failed) != nil {
			return fmt.Errorf("失败隔离记录损坏，拒绝发布候选")
		}
		if _, found := failed[m.SHA256]; found {
			return fmt.Errorf("此候选曾启动失败，等待后续修复版本")
		}
	}
	slot := filepath.Join(dir, m.SHA256)
	if err := os.MkdirAll(slot, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(slot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("运行槽不是独立目录")
	}
	dest := filepath.Join(slot, "pvn-node")
	// 不能覆盖正在执行的槽；先写临时文件再 rename。
	if err := copyFile(source, dest+".part"); err != nil {
		return err
	}
	if err := os.Chmod(dest+".part", 0755); err != nil {
		return err
	}
	if err := containerSyncFile(dest + ".part"); err != nil {
		return err
	}
	if err := os.Rename(dest+".part", dest); err != nil {
		return err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := containerWriteDurable(filepath.Join(slot, "manifest.json"), raw); err != nil {
		return err
	}
	if err := containerSyncDir(dir); err != nil {
		return err
	}
	return containerWriteDurable(filepath.Join(dir, "candidate.json"), raw)
}
