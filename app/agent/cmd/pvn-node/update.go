// 自更新支持：
//   - 程序启动后自动向 GitHub Releases 查询最新版本（每次启动检查一次），
//     预先拉取发行说明，控制台「更新」弹框直接展示、无需等待；
//   - /api/update    查询检查结果（含 has_update / notes / asset / checked_at）；
//     ?force=1 绕过缓存强制重查（打开弹框即触发，发布新版本后立刻可见）；
//   - /api/update/apply  下载对应平台发行包 → sha256 校验 → 原子替换自身 → 重启；
//   - /api/restart   以原参数重启程序；
//   - /api/quit      退出程序。
//
// 缓存策略：检查结果复用 updateCacheTTL（3 分钟）；超窗口或 force 时同步重查，
// 同步超时 updateSyncTimeout（6 秒，超时返回上次缓存并标记 timed_out），
// 失败也刷新时间戳以退避，避免网络抖动时反复打 GitHub API。
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
	"errors"
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

	"github.com/ayflying/pvn/pkg/selfupdate"
)

const updateRepoAPI = "https://api.github.com/repos/ayflying/lanet/releases/latest"

// updateCacheTTL 检查结果的复用窗口：窗口内直接返回缓存，避免频繁打 GitHub API。
// 窗口外（或带 ?force=1）会同步重新检查，因此发布新版本后用户「打开更新弹框」
// 即可立刻看到，不必重启或等待 P2P 巡检。
const updateCacheTTL = 3 * time.Minute

// updateSyncTimeout 同步检查（页面请求触发）的超时：GitHub API 正常 < 1s，
// 6 秒足够；超时则放弃本次检查、返回缓存结果，避免用户点开弹框干等。
const updateSyncTimeout = 6 * time.Second

// updateStartTimeout 启动时后台检查的超时（不阻塞任何交互，可放宽）。
const updateStartTimeout = 20 * time.Second

type updateState struct {
	mu        sync.Mutex
	checked   bool
	hasUpdate bool
	needToken bool
	errMsg    string
	current   string
	latest    string
	notes     string
	assetURL  string // 浏览器下载地址（仅展示）
	assetAPI  string // API 资产端点（实际下载）
	sumsAPI   string // sha256sums.txt API 资产端点
	assetName string
	checkedAt time.Time
	token     string
}

var upd = &updateState{current: version}

var updateCheckFlight struct {
	sync.Mutex
	call *updateCheckCall
}

type updateCheckCall struct {
	done chan struct{}
	err  error
}

// 并发 force 与启动检查共享一次请求；等待者取消不影响其他等待者。
func sharedCheckUpdate(ctx context.Context, force bool) error {
	updateCheckFlight.Lock()
	call := updateCheckFlight.call
	if call == nil {
		upd.mu.Lock()
		fresh := upd.checked && time.Since(upd.checkedAt) <= updateCacheTTL
		upd.mu.Unlock()
		if !force && fresh {
			updateCheckFlight.Unlock()
			return nil
		}
		call = &updateCheckCall{done: make(chan struct{})}
		updateCheckFlight.call = call
		go func() {
			workCtx, cancel := context.WithTimeout(context.Background(), updateStartTimeout)
			defer cancel()
			call.err = checkUpdate(workCtx)
			updateCheckFlight.Lock()
			updateCheckFlight.call = nil
			close(call.done)
			updateCheckFlight.Unlock()
		}()
	}
	updateCheckFlight.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-call.done:
		return call.err
	}
}

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
	return map[string]http.HandlerFunc{
		"GET /api/update": func(w http.ResponseWriter, r *http.Request) {
			// force=1：跳过缓存强制重新检查（前端「检查更新」按钮用）。
			// 其余情况：未检查过或缓存过期时同步补查一次，保证「点开弹框
			// 即有相对新鲜的结果」；缓存窗口内直接返回，避免打爆 GitHub API。
			force := r.URL.Query().Get("force") == "1"
			upd.mu.Lock()
			checked, checkedAt := upd.checked, upd.checkedAt
			upd.mu.Unlock()
			// 注意：失败路径也会刷新 checkedAt（见 fail），因此失败同样按 TTL 退避。
			timedOut := false
			if force || !checked || time.Since(checkedAt) > updateCacheTTL {
				ctx, cancel := context.WithTimeout(r.Context(), updateSyncTimeout)
				if err := sharedCheckUpdate(ctx, force); err != nil && ctx.Err() != nil {
					timedOut = true // 超时：本次放弃，下面返回上次缓存结果
				}
				cancel()
			}
			checked, hasUpdate, needToken, latest, notes, _, assetName, errMsg := upd.snapshot()
			upd.mu.Lock()
			at := upd.checkedAt
			upd.mu.Unlock()
			writeJSONLocal(w, http.StatusOK, map[string]any{
				"checked":     checked,
				"has_update":  hasUpdate,
				"need_token":  needToken,
				"error":       errMsg,
				"current":     upd.current,
				"latest":      latest,
				"notes":       notes,
				"asset":       assetName,
				"checked_at":  at.Format(time.RFC3339),
				"cache_ttl_s": int(updateCacheTTL.Seconds()),
				"timed_out":   timedOut,
			})
		},
		"POST /api/update/apply": func(w http.ResponseWriter, r *http.Request) {
			if err := applyUpdate(); err != nil {
				log.Printf("[update] 应用更新失败: %v", err)
				status := http.StatusInternalServerError
				if errors.Is(err, errUpdateInFlight) {
					// 已有更新在途（另一条路径/上一轮还没重启）：这是并发拒绝，
					// 不是服务故障，回 409 让前端提示「更新进行中」而不是报错。
					status = http.StatusConflict
				}
				writeJSONLocal(w, status, map[string]string{"error": err.Error()})
				return
			}
			writeJSONLocal(w, http.StatusOK, map[string]any{"restarting": true, "update_pending": true})
			restartSelf(1500 * time.Millisecond) // 留出响应送达时间；启动入口消费暂存更新
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
		ctx, cancel := context.WithTimeout(context.Background(), updateStartTimeout)
		defer cancel()
		if err := sharedCheckUpdate(ctx, false); err != nil {
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
// 无论成功失败都会更新 checkedAt（失败时带 errMsg），使调用方可以按
// updateCacheTTL 退避重试，不会因网络抖动而在每次请求里反复打 GitHub。
func checkUpdate(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, updateRepoAPI, nil)
	if err != nil {
		upd.fail(err)
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if upd.token != "" {
		req.Header.Set("Authorization", "Bearer "+upd.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		upd.fail(err)
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
		return upd.fail(fmt.Errorf("GitHub API 返回 %d", resp.StatusCode))
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
		Assets  []struct {
			Name               string `json:"name"`
			URL                string `json:"url"` // API 资产端点（私有仓库下载必须走这里）
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	data, err := readUpdateHTTPBody(resp.Body, 2<<20)
	if err != nil {
		return upd.fail(err)
	}
	if err := json.Unmarshal(data, &rel); err != nil {
		upd.fail(err)
		return err
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	res := updateResult{latest: latest, notes: rel.Body}
	if versionLess(upd.current, latest) {
		res.hasUpdate = true
		want := fmt.Sprintf("lanet-%s-%s-%s", latest, runtime.GOOS, runtime.GOARCH)
		for _, a := range rel.Assets {
			if a.Name == want+".zip" || a.Name == want+".tar.gz" {
				res.assetURL, res.assetAPIURL = a.BrowserDownloadURL, a.URL
				res.assetName = a.Name
			}
			if a.Name == "sha256sums.txt" {
				res.sumsAPIURL = a.URL
			}
		}
		if res.assetAPIURL == "" {
			res.errMsg = fmt.Sprintf("最新版 %s 未提供 %s/%s 的发行包", latest, runtime.GOOS, runtime.GOARCH)
			res.hasUpdate = false
		} else if res.sumsAPIURL == "" {
			res.errMsg = "最新版缺少 sha256sums.txt，拒绝不完整发行包"
			res.hasUpdate = false
		}
	}
	upd.finish(res, "")
	return nil
}

type updateResult struct {
	hasUpdate   bool
	needToken   bool
	latest      string
	notes       string
	assetURL    string // 浏览器下载地址（仅展示用，私有仓库不能用它下载）
	assetAPIURL string // API 资产端点（实际下载走这里，带 Token + octet-stream）
	sumsAPIURL  string // sha256sums.txt 的 API 资产端点
	assetName   string
	errMsg      string
}

func (u *updateState) finish(res updateResult, errMsg string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.checked = true
	u.checkedAt = time.Now()
	u.hasUpdate, u.needToken = res.hasUpdate, res.needToken
	u.latest, u.notes = res.latest, res.notes
	u.assetURL, u.assetName = res.assetURL, res.assetName
	u.assetAPI, u.sumsAPI = res.assetAPIURL, res.sumsAPIURL
	u.errMsg = errMsg
}

// fail 记录一次检查失败：更新时间戳使调用方按 TTL 退避，并保留错误信息。
// 失败不影响上次成功拿到的版本信息（latest 等），页面仍可展示。
func (u *updateState) fail(err error) error {
	u.mu.Lock()
	u.checked = true
	u.checkedAt = time.Now()
	u.errMsg = err.Error()
	u.mu.Unlock()
	return err
}

// downloadTargets 返回应用更新所需的真实下载地址（API 资产端点）。
func (u *updateState) downloadTargets() (assetAPI, sumsAPI, assetName string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.assetAPI, u.sumsAPI, u.assetName
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
//
// 与 P2P 自动更新共用 updateGate（见 p2pupdate.go）：两条路径不会同时替换
// 同一个 exe；已有一轮更新在途时直接拒绝——避免「升级窗口里又点一次」造成
// 重复下载、重复替换、反复重启。
func applyUpdate() (err error) {
	if !acquireUpdate() {
		return errUpdateInFlight
	}
	landed := false
	defer func() {
		if !landed {
			releaseUpdate() // 没落地：释放闸门，允许用户重试
		}
	}()
	_, hasUpdate, _, _, _, _, _, _ := upd.snapshot()
	if !hasUpdate {
		return fmt.Errorf("没有可应用的更新")
	}
	assetAPI, sumsAPI, assetName := upd.downloadTargets()
	if assetAPI == "" {
		return fmt.Errorf("没有可下载的更新资产（请重新检查更新）")
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
	sum, err := downloadToFile(assetAPI, archivePath, upd.token)
	if err != nil {
		return fmt.Errorf("下载失败: %w", err)
	}
	// sha256 校验（对比发行包内 sha256sums.txt）；自动更新安全要求 fail-closed。
	if sumsAPI == "" {
		return fmt.Errorf("最新版缺少 sha256sums.txt，拒绝更新")
	}
	if err = verifySHA256(sumsAPI, assetName, sum, upd.token); err != nil {
		return fmt.Errorf("校验失败: %w", err)
	}
	log.Printf("[update] sha256 校验通过 %s", hex.EncodeToString(sum)[:16]+"…")

	// 解出 lanet(.exe)（Windows 同时解出 wintun.dll，若被占用则跳过）。
	newExe := filepath.Join(workDir, "lanet-new"+extOf())
	if err = extractBinary(archivePath, assetName, newExe, exeDir); err != nil {
		return fmt.Errorf("解压失败: %w", err)
	}

	// 更新先落到安装目录的确定性暂存名；运行中的程序不替换自身。
	// 下次进程启动（普通重启或 Windows 服务重启）时会消费该候选。
	stagedPath := pendingUpdatePath(exePath)
	if err := stageNewBinary(newExe, exePath); err != nil {
		return fmt.Errorf("暂存更新失败: %w", err)
	}
	log.Printf("[update] 新程序已校验并暂存：%s（安装包 %s）；将在下次启动时切换", stagedPath, assetName)
	landed = true
	return nil
}

// sha256sumsURL 已废弃：私有仓库的 browser_download_url 无法带 Token 下载，
// 现一律走 checkUpdate 时缓存的 API 资产端点（sumsAPIURL）。

func verifySHA256(sumsURL, assetName string, sum []byte, token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sumsURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream") // API 资产端点：拿文件内容而非 JSON 元数据
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("获取 sha256sums.txt 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := readUpdateHTTPBody(resp.Body, 1<<20)
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

func readUpdateHTTPBody(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("更新响应超过大小上限")
	}
	return data, nil
}

func downloadToFile(url, path, token string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// 私有仓库发行包必须带 Token（API 资产端点 302 到签名对象存储，
	// 跳转后不再需要鉴权，多余的头无副作用）。
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/octet-stream") // API 资产端点：拿文件内容而非 JSON 元数据
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
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, (512<<20)+1))
	if err != nil {
		return nil, err
	}
	if n > 512<<20 {
		return nil, errors.New("更新发行包超过512MiB上限")
	}
	if err := f.Close(); err != nil {
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
	var distManifest []byte
	for _, f := range r.File {
		base := filepath.Base(f.Name)
		switch {
		case base == zipName("lanet"):
			if err = copyZipEntry(f, destExe); err != nil {
				return err
			}
		case base == selfupdate.DistManifestName && distManifest == nil:
			// 包内签名清单先暂存：它要与「刚解出的新程序」比对 sha256，
			// 而 zip 内条目顺序不定，只能等循环结束后再落盘。
			if raw, err := readZipEntry(f); err == nil {
				distManifest = raw
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
	installDistManifest(distManifest, exeDir, destExe)
	return nil
}

// readZipEntry 读出一个 zip 条目的全部内容（清单只有几百字节）。
func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return readUpdateHTTPBody(rc, 1<<20)
}

// distManifestInstaller 落盘包内签名清单的实现（测试注入点：真实实现要求
// 清单出自发布私钥，而私钥只在 CI 里，测试无从构造合法凭证）。
var distManifestInstaller = selfupdate.InstallDistManifest

// installDistManifest 把发行包内的签名清单落盘为运行期分发凭证
// （update-manifest.json），使本节点重启后能作为 P2P 种子对外分发新版本。
// 失败不算更新失败：程序已经换好了，只是本节点暂时只做升级请求方。
func installDistManifest(raw []byte, exeDir, destExe string) {
	if len(raw) == 0 {
		return
	}
	if err := distManifestInstaller(raw,
		filepath.Join(exeDir, "update-manifest.json"), destExe); err != nil {
		log.Printf("[update] 包内分发清单不可用（%v）：本节点暂不作为 P2P 分发源", err)
		return
	}
	log.Printf("[update] 分发凭证已落盘：重启后本节点可作为 P2P 新版本种子")
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
	found := false
	var distManifest []byte
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch base := filepath.Base(hdr.Name); {
		case base == zipName("lanet"):
			// 不提前 return：同一个包里还有签名清单要落盘。
			if err := writeAtomic(destExe, tr); err != nil {
				return err
			}
			found = true
		case base == selfupdate.DistManifestName && distManifest == nil:
			if raw, err := io.ReadAll(tr); err == nil {
				distManifest = raw
			}
		}
	}
	if !found {
		return fmt.Errorf("包内未找到 %s", zipName("lanet"))
	}
	installDistManifest(distManifest, exeDir, destExe)
	return nil
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

// installNewBinary 把解出的新程序落到 exePath，兼容「旧程序正在运行」场景。
//
// Windows：lanet.exe 被进程映像锁住（本节点/托盘伴侣都在跑它），Go 的
// os.Rename 对已存在目标会先 os.Remove——删除需要目标的 DELETE 访问权，
// 被锁即 Access denied（0.5.72 升级实证）。改用 Win32 MoveFileEx 的
// REPLACE_EXISTING：它走目录项原子替换，不打开目标文件，正在运行的 exe
// 也能换名（新程序下次启动生效）。非 Windows 无此锁，直接改名腾位再写入。
func installNewBinary(newExe, exePath string) error {
	if runtime.GOOS == "windows" {
		return replaceLockedBinary(newExe, exePath)
	}
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

func restoreBackup(exePath string) error {
	backup := exePath + ".rollback"
	if _, err := os.Stat(backup); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return replaceLockedBinary(backup, exePath)
	}
	if err := os.Remove(exePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(backup, exePath)
}

// stageNewBinary 将候选程序原子落到安装目录并写待更新标记；旧程序不被触碰。
// 候选及标记同目录，避免跨卷 rename 导致非原子复制。
//
// 一致性：先清掉上一轮标记，再发布候选，最后原子写新标记。任何一步中途失败
// 都只留下「无标记」状态，绝不会残留「标记指向已被覆盖/删除的候选」——那种
// 残留会让之后每次启动都判为待更新、却永远应用失败，形成无法自愈的启动循环
// （旧实现在写新标记失败时删候选却留着旧标记，正是这个坑）。
// 失败即放弃这一轮：当前版本继续运行，下次检查会重新下载暂存。
func stageNewBinary(newExe, exePath string) error {
	pendingExe := pendingUpdatePath(exePath)
	if err := removeIfExists(pendingUpdateMarkerPath(exePath)); err != nil {
		return fmt.Errorf("清理上一轮待更新标记失败: %w", err)
	}
	tmp := pendingExe + ".part"
	if err := copyFile(newExe, tmp); err != nil {
		return fmt.Errorf("复制待更新程序失败: %w", err)
	}
	if err := os.Rename(tmp, pendingExe); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("发布待更新程序失败: %w", err)
	}
	data, err := os.ReadFile(pendingExe)
	if err != nil {
		_ = os.Remove(pendingExe)
		return err
	}
	sum := sha256.Sum256(data)
	marker, err := json.Marshal(pendingUpdateMarker{SHA256: hex.EncodeToString(sum[:])})
	if err != nil {
		_ = os.Remove(pendingExe)
		return err
	}
	if err := writeAtomicBytes(pendingUpdateMarkerPath(exePath), marker, 0o600); err != nil {
		_ = os.Remove(pendingExe)
		return fmt.Errorf("写入待更新标记失败: %w", err)
	}
	return nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// dropPendingUpdate 丢弃一对「标记 + 候选」残留。只在确认无法应用时调用：
// 保留它们毫无意义，只会让每次启动都白跑一趟更新辅助进程。
func dropPendingUpdate(exePath string) {
	_ = removeIfExists(pendingUpdateMarkerPath(exePath))
	_ = removeIfExists(pendingUpdatePath(exePath))
	_ = removeIfExists(pendingUpdatePath(exePath) + ".part")
}

type pendingUpdateMarker struct {
	SHA256 string `json:"sha256"`
}

func pendingUpdatePath(exePath string) string       { return exePath + ".pending" }
func pendingUpdateMarkerPath(exePath string) string { return exePath + ".pending.json" }

func writeAtomicBytes(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".part"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// applyPendingUpdate 在旧程序已退出的更新辅助进程中调用；摘要不匹配时拒绝切换。
//
// 自愈：标记损坏、标记非法、候选缺失都属于「永远应用不了」的残留状态，直接丢弃
// （不安装任何东西，因此仍然 fail-closed），否则每次启动都会被判为待更新。
// 摘要不匹配则相反——保留现场供排查，只拒绝切换。
func applyPendingUpdate(exePath string) (bool, error) {
	markerPath, candidate := pendingUpdateMarkerPath(exePath), pendingUpdatePath(exePath)
	markerBytes, err := os.ReadFile(markerPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var marker pendingUpdateMarker
	if err := json.Unmarshal(markerBytes, &marker); err != nil {
		dropPendingUpdate(exePath)
		return false, fmt.Errorf("待更新标记损坏，已丢弃: %w", err)
	}
	if len(marker.SHA256) != sha256.Size*2 {
		dropPendingUpdate(exePath)
		return false, errors.New("待更新标记非法（摘要长度错误），已丢弃")
	}
	f, err := os.Open(candidate)
	if err != nil {
		dropPendingUpdate(exePath)
		return false, fmt.Errorf("待更新程序缺失，已丢弃: %w", err)
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	closeErr := f.Close()
	if copyErr != nil {
		return false, copyErr
	}
	if closeErr != nil {
		return false, closeErr
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), marker.SHA256) {
		return false, errors.New("待更新程序 SHA256 不匹配，拒绝切换（保留现场待排查）")
	}
	backup := exePath + ".rollback"
	if err := copyFile(exePath, backup); err != nil {
		return false, fmt.Errorf("备份旧程序失败: %w", err)
	}
	if err := installNewBinary(candidate, exePath); err != nil {
		_ = os.Remove(backup)
		return false, err
	}
	if err := os.Remove(markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		if rollbackErr := restoreBackup(exePath); rollbackErr != nil {
			return false, fmt.Errorf("清理待更新标记失败且回滚失败（%v）: %w", rollbackErr, err)
		}
		return false, fmt.Errorf("清理待更新标记失败，已回滚: %w", err)
	}
	_ = os.Remove(candidate)
	return true, nil
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
		if isServiceProcess() {
			log.Printf("[service] 请求 Windows 服务管理器重启 Lanet")
			if err := restartWindowsService(); err != nil {
				log.Printf("[service] 重启服务失败: %v", err)
			}
			return
		}
		exe := selfExe()
		log.Printf("[node] 重启程序: %s %v", exe, os.Args[1:])
		if _, err := os.Stat(pendingUpdateMarkerPath(exe)); err == nil {
			if err := spawnUpdateHelper(); err != nil {
				log.Printf("[node] 更新辅助进程启动失败: %v", err)
				return
			}
			activeSingleton.release()
			os.Exit(0)
		}
		// 必须先放单实例锁再拉起新进程：spawnSelf 是 Start 后立刻返回、本进程
		// 紧接着 os.Exit，新进程起来时旧进程往往还活着几十毫秒，不提前释放会
		// 让新进程把自己判成「双开」而拒绝启动——重启直接变成彻底停服。
		// （服务模式走 SCM stop/start，SCM 会等本进程完全退出，无需手动放锁。）
		activeSingleton.release()
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
