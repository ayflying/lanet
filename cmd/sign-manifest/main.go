// sign-manifest 发布签名工具：对单个平台的二进制生成带 Ed25519 签名的
// 版本清单（manifest 分片），CI 发版流水线用它给每个产物签名。
//
// 私钥来源：GitHub Actions Secrets（SELFUPDATE_SIGNING_KEY，base64 单行），
// 经环境变量或文件传入。私钥绝不入库。
//
// 用法：
//
//	go run ./cmd/sign-manifest -key <b64文件或环境变量 KEY> \
//	    -version 0.5.0 -platform windows/amd64 -file dist/lanet.exe \
//	    -out dist/manifest-windows-amd64.json
package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ayflying/pvn/pkg/selfupdate"
)

func main() {
	keyPath := flag.String("key", "", "PEM 私钥文件路径；也可经 SELFUPDATE_SIGNING_KEY 环境变量传入 PEM 全文")
	version := flag.String("version", "", "版本号（与 VERSION 文件一致）")
	platform := flag.String("platform", "", "平台标识 GOOS/GOARCH，如 windows/amd64")
	file := flag.String("file", "", "待签名二进制路径")
	out := flag.String("out", "", "输出 manifest 分片路径（默认 stdout）")
	flag.Parse()

	privB64 := os.Getenv("SELFUPDATE_SIGNING_KEY")
	if privB64 == "" && *keyPath != "" {
		data, err := os.ReadFile(*keyPath)
		if err != nil {
			fatal("读私钥文件: %v", err)
		}
		privB64 = string(data)
	}
	privB64 = strings.TrimSpace(privB64)
	if privB64 == "" || *version == "" || *platform == "" || *file == "" {
		fmt.Fprintln(os.Stderr, "参数缺失：-version/-platform/-file 必填，私钥经 -key 或 SELFUPDATE_SIGNING_KEY 提供")
		os.Exit(2)
	}

	priv, err := parsePrivateKey(privB64)
	if err != nil {
		fatal("私钥非法: %v", err)
	}
	sum, err := selfupdate.FileSHA256(*file)
	if err != nil {
		fatal("计算 %s sha256: %v", *file, err)
	}
	fi, err := os.Stat(*file)
	if err != nil {
		fatal("stat %s: %v", *file, err)
	}
	m := selfupdate.Manifest{
		Version:  *version,
		Platform: *platform,
		Size:     fi.Size(),
		SHA256:   sum,
	}
	if err = selfupdate.SignManifest(ed25519.PrivateKey(priv), &m); err != nil {
		fatal("签名失败: %v", err)
	}
	if !m.Verify(selfupdate.ReleasePublicKey) {
		fatal("自验失败：签名与内置公钥不匹配（密钥对不一致？）")
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	if *out == "" {
		fmt.Println(string(data))
		return
	}
	if err = os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fatal("创建输出目录: %v", err)
	}
	if err = os.WriteFile(*out, data, 0o644); err != nil {
		fatal("写 %s: %v", *out, err)
	}
	fmt.Printf("signed %s (%s, %d bytes, sha256=%s…)\n", *file, *platform, m.Size, m.SHA256[:12])
}

func fatal(f string, args ...any) {
	fmt.Fprintf(os.Stderr, "sign-manifest: "+f+"\n", args...)
	os.Exit(1)
}

// parsePrivateKey 解析 PKCS#8 PEM 私钥（-----BEGIN PRIVATE KEY-----，
// openssl 标准格式）。GitHub Secret SELFUPDATE_SIGNING_KEY 存放 PEM 全文。
func parsePrivateKey(s string) (ed25519.PrivateKey, error) {
	s = strings.TrimSpace(s)
	blk, _ := pem.Decode([]byte(s))
	if blk == nil {
		return nil, fmt.Errorf("PEM 解析失败：私钥应为 PKCS#8 PEM 格式")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("PKCS#8 解析失败: %w", err)
	}
	key, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("PEM 内不是 Ed25519 私钥（got %T）", k)
	}
	return key, nil
}
