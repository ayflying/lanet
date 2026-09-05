// 自更新支持：
//   - 程序启动后自动向 GitHub Releases 查询最新版本（每次启动检查一次），
//     预先拉取发行说明，控制台「更新」弹框直接展示、无需等待；
//   - /api/update    查询检查结果（含 has_update / notes / asset 信息）；
//   - /api/update/apply  下载对应平台发行包 → sha256 校验 → 原子替换自身 → 重启；
//   - /api/restart   以原参数重启程序；
//   - /api/quit      退出程序。
//
// 仓库为私有时的限制：GitHub API 需要 Token —— 环境变量 GITHUB_TOKEN 或
// lanet.json 的 github_token 字段（只需 contents:read 权限）；未提供时
// 检查结果返回 need_token 提示。
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const updateRepoAPI = "https://api.github.com/repos/ayflying/lanet/releases/latest"

type updateState struct {
	mu        sync.Mutex
	checked   bool
	hasUpdate bool
	needToken bool
	errMsg    string
	current   string
	latest    string
	notes     string
	assetURL  string // 发行包下载地址（当前平台）
	assetName string
	checkedAt time.Time
	token     string
}

var upd = &updateState{current: version}

// readConfigToken 从 lanet.json 读 github_token（检查私有仓库更新用）。
func readConfigToken(path string) string {
	var nc nodeConfig
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &nc)
	}
	return nc.GitHubToken
}

// updateRoutes 控制台扩展路由：更新检查 / 应用更新 / 重启 / 退出。
func updateRoutes(cancel context.CancelFunc) map[string]http.HandlerFunc {
	var syncCheckMu sync.Mutex
	return map[string]http.HandlerFunc{
		"GET /api/update": func(w http.ResponseWriter, r *http.Request) {
			// 尚未检查过（启动检查未完成/失败）时同步补查一次，页面即点即有结果。
			syncCheckMu.Lock()
			upd.mu.Lock()
			checked, checkedAt := upd.checked, upd.checkedAt
			upd.mu.Unlock()
			if !checked || time.Since(checkedAt) > time.Hour {
				ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
				_ = checkUpdate(ctx)
				cancel()
			}
			syncCheckMu.Unlock()
			checked, hasUpdate, needToken, latest, notes, _, assetName, errMsg := upd.snapshot()
			writeJSONLocal(w, http.StatusOK, map[string]any{
				"checked":    checked,
				"has_update": hasUpdate,
				"need_token": needToken,
				"error":      errMsg,
				"current":    upd.current,
				"latest":     latest,
				"notes":      notes,
				"asset":      assetName,
			})
		},
		"POST /api/update/apply": func(w http.ResponseWriter, r *http.Request) {
			if err := applyUpdate(); err != nil {
				log.Printf("[update] 应用更新失败: %v", err)
				writeJSONLocal(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSONLocal(w, http.StatusOK, map[string]any{"restarting": true})
			restartSelf(1500 * time.Millisecond) // 留出响应送达时间
		},
		"POST /api/restart": func(w http.ResponseWriter, r *http.Request) {
			writeJSONLocal(w, http.StatusOK, map[string]any{"restarting": true})
			restartSelf(800 * time.Millisecond)
		},
		"POST /api/quit": func(w http.ResponseWriter, r *http.Request) {
			writeJSONLocal(w, http.StatusOK, map[string]any{"quitting": true})
			quitSelf(800*time.Millisecond, cancel)
		},
	}
}

// StartUpdateCheck 启动时后台检查一次更新。
func StartUpdateCheck(token string) {
	upd.token = token
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := checkUpdate(ctx); err != nil {
			log.Printf("[update] 检查更新失败: %v", err)
		}
	}()
	// 清理上次更新遗留的旧程序文件。
	old := selfExe() + ".old"
	if _, err := os.Stat(old); err == nil {
		_ = os.Remove(old)
	}
}

// checkUpdate 查询 GitHub 最新 Release 并与当前版本比较。
func checkUpdate(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, updateRepoAPI, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if upd.token != "" {
		req.Header.Set("Authorization", "Bearer "+upd.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusForbidden {
		upd.finish(updateResult{needToken: resp.StatusCode != http.StatusForbidden},
			fmt.Sprintf("GitHub API 返回 %d（仓库私有需提供只读 Token：环境变量 GITHUB_TOKEN 或 lanet.json 的 github_token）", resp.StatusCode))
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API 返回 %d", resp.StatusCode)
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return err
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	res := updateResult{latest: latest, notes: rel.Body}
	if versionLess(upd.current, latest) {
		res.hasUpdate = true
		want := fmt.Sprintf("lanet-%s-%s-%s", latest, runtime.GOOS, runtime.GOARCH)
		for _, a := range rel.Assets {
			if a.Name == want+".zip" || a.Name == want+".tar.gz" {
				res.assetURL, res.assetName = a.BrowserDownloadURL, a.Name
				break
			}
		}
		if res.assetURL == "" {
			res.errMsg = fmt.Sprintf("最新版 %s 未提供 %s/%s 的发行包", latest, runtime.GOOS, runtime.GOARCH)
			res.hasUpdate = false
		}
	}
	upd.finish(res, "")
	return nil
}

type updateResult struct {
	hasUpdate bool
	needToken bool
	latest    string
	notes     string
	assetURL  string
	assetName string
	errMsg    string
}

func (u *updateState) finish(res updateResult, errMsg string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.checked = true
	u.checkedAt = time.Now()
	u.hasUpdate, u.needToken = res.hasUpdate, res.needToken
	u.latest, u.notes = res.latest, res.notes
	u.assetURL, u.assetName = res.assetURL, res.assetName
	u.errMsg = errMsg
}

// versionLess 三段式版本比较：a < b 返回 true。非法段按 0 处理。
func versionLess(a, b string) bool {
	pa, pb := parseSemver(a), parseSemver(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func parseSemver(v string) [3]int {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	for i, part := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		n := 0
		for _, c := range part {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out
}

func (u *updateState) snapshot() (checked, hasUpdate, needToken bool, latest, notes, assetURL, assetName, errMsg string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.checked, u.hasUpdate, u.needToken, u.latest, u.notes, u.assetURL, u.assetName, u.errMsg
}

// applyUpdate 下载发行包并校验，替换自身后重启。失败返回错误信息给页面。
func applyUpdate() error {
	_, hasUpdate, _, _, _, assetURL, assetName, _ := upd.snapshot()
	if !hasUpdate || assetURL == "" {
		return fmt.Errorf("没有可应用的更新")
	}
	exePath := selfExe()
	exeDir := filepath.Dir(exePath)
	workDir, err := os.MkdirTemp(exeDir, "update-")
	if err != nil {
		return fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(workDir)

	log.Printf("[update] 开始下载 %s", assetName)
	archivePath := filepath.Join(workDir, assetName)
	sum, err := downloadToFile(assetURL, archivePath, upd.token)
	if err != nil {
		return fmt.Errorf("下载失败: %w", err)
	}
	// sha256 校验（对比发行包内 sha256sums.txt）。
	if sumsURL := sha256sumsURL(assetURL, assetName); sumsURL != "" {
		if err = verifySHA256(sumsURL, assetName, sum, upd.token); err != nil {
			return fmt.Errorf("校验失败: %w", err)
		}
		log.Printf("[update] sha256 校验通过 %s", hex.EncodeToString(sum)[:16]+"…")
	}

	// 解出 lanet(.exe)（Windows 同时解出 wintun.dll，若被占用则跳过）。
	newExe := filepath.Join(workDir, "lanet-new"+extOf())
	if err = extractBinary(archivePath, assetName, newExe, exeDir); err != nil {
		return fmt.Errorf("解压失败: %w", err)
	}

	log.Printf("[update] 替换程序并重启（%s -> %s）", assetName, exePath)
	// 正在运行的 exe 不能直接覆盖：改名腾位 → 放入新程序 → 拉起新进程 → 退出。
	oldPath := exePath + ".old"
	_ = os.Remove(oldPath)
	if err := os.Rename(exePath, oldPath); err != nil {
		return fmt.Errorf("旧程序改名失败: %w", err)
	}
	if err := copyFile(newExe, exePath); err != nil {
		_ = os.Rename(oldPath, exePath) // 回滚
		return fmt.Errorf("写入新程序失败: %w", err)
	}
	return nil
}

// sha256sumsURL 由发行包地址推出 sha256sums.txt 地址（同一 Release 目录）。
func sha256sumsURL(assetURL, assetName string) string {
	if !strings.Contains(assetURL, "/releases/download/") {
		return ""
	}
	return strings.TrimSuffix(assetURL, assetName) + "sha256sums.txt"
}

func verifySHA256(sumsURL, assetName string, sum []byte, token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sumsURL, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err // 拿不到校验和时不阻断更新（下载本身走 HTTPS）
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	want := hex.EncodeToString(sum)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == assetName {
			if !strings.EqualFold(fields[0], want) {
				return fmt.Errorf("sha256 不匹配（期望 %s，实际 %s）", fields[0], want)
			}
			return nil
		}
	}
	return fmt.Errorf("sha256sums.txt 中找不到 %s", assetName)
}

func downloadToFile(url, path, token string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// 私有仓库的发行包必须带 Token（browser_download_url 302 到签名对象存储，
	// 跳转后不再需要鉴权，多余的头无副作用）。
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// extractBinary 从发行包解出主程序；windows 额外尝试解出 wintun.dll（失败忽略）。
func extractBinary(archivePath, assetName, destExe, exeDir string) error {
	if strings.HasSuffix(assetName, ".zip") {
		return extractFromZip(archivePath, destExe, exeDir)
	}
	return extractFromTarGz(archivePath, destExe, exeDir)
}

func zipName(base string) string { return base + extOf() }

func extractFromZip(path, destExe, exeDir string) error {
	r, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer r.Close()
	var wintunDone bool
	for _, f := range r.File {
		base := filepath.Base(f.Name)
		switch {
		case base == zipName("lanet"):
			if err = copyZipEntry(f, destExe); err != nil {
				return err
			}
		case base == "wintun.dll" && !wintunDone:
			// 正在运行的进程可能锁定 dll：失败忽略（dll 极少随版本变化）。
			if err := copyZipEntry(f, filepath.Join(exeDir, "wintun.dll")); err == nil {
				wintunDone = true
			}
		}
	}
	if _, err := os.Stat(destExe); err != nil {
		return fmt.Errorf("包内未找到 %s", zipName("lanet"))
	}
	return nil
}

func copyZipEntry(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	return writeAtomic(dest, rc)
}

func extractFromTarGz(path, destExe, exeDir string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if filepath.Base(hdr.Name) == zipName("lanet") {
			return writeAtomic(destExe, tr)
		}
	}
	return fmt.Errorf("包内未找到 %s", zipName("lanet"))
}

func writeAtomic(dest string, r io.Reader) error {
	tmp := dest + ".part"
	fh, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err = io.Copy(fh, r); err != nil {
		fh.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err = fh.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeAtomic(dst, in)
}

func selfExe() string {
	exe, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}

func extOf() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// spawnSelf 以当前参数启动一个新进程（独立进程组，不随父进程退出）。
// Windows：新 exe 内嵌 requireAdministrator 清单，非提权父进程 CreateProcess
// 会报 740，此时降级 ShellExecute "runas"（已提权则无感，未提权弹 UAC 确认）。
func spawnSelf() error {
	if elevated, err := spawnSelfWindows(); elevated || err != nil {
		return err // Windows 路径已处理（含提权降级）
	}
	return nil
}

// restartSelf 延迟拉起新进程后退出当前进程（延迟用于让 HTTP 响应先送达）。
func restartSelf(delay time.Duration) {
	go func() {
		time.Sleep(delay)
		exe := selfExe()
		log.Printf("[node] 重启程序: %s %v", exe, os.Args[1:])
		if err := spawnSelf(); err != nil {
			log.Printf("[node] 重启失败: %v", err)
			os.Exit(1)
		}
		os.Exit(0)
	}()
}

// quitSelf 延迟退出程序。
func quitSelf(delay time.Duration, cancel context.CancelFunc) {
	go func() {
		time.Sleep(delay)
		log.Printf("[node] 控制台请求退出")
		cancel()
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}()
}
