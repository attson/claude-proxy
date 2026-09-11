package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	repoOwner = "attson"
	repoName  = "claude-proxy"
)

// selfUpdate 实现 `claude-proxy update [--check]`:
//   - 查 GitHub latest release,与当前 version 比对;
//   - --check 只报告是否有新版,不下载;
//   - 否则下载对应平台产物、校验 sha256、原子替换自身。
func selfUpdate(checkOnly bool) int {
	fmt.Printf("当前版本: %s\n", version)
	rel, err := fetchLatestRelease()
	if err != nil {
		fmt.Fprintf(os.Stderr, "查询最新版本失败: %v\n", err)
		return 1
	}
	latest := rel.TagName
	fmt.Printf("最新版本: %s\n", latest)

	if !isNewer(latest, version) {
		fmt.Println("已是最新版本,无需更新。")
		return 0
	}
	if checkOnly {
		fmt.Printf("有新版本可用: %s -> %s (运行 `claude-proxy update` 升级)\n", version, latest)
		return 0
	}

	assetName := platformAssetName(latest)
	assetURL := findAsset(rel, assetName)
	if assetURL == "" {
		fmt.Fprintf(os.Stderr, "未找到匹配当前平台的产物: %s\n", assetName)
		return 1
	}
	fmt.Printf("下载 %s ...\n", assetName)
	archive, err := downloadAsset(assetURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "下载失败: %v\n", err)
		fmt.Fprintf(os.Stderr, "\n可能是网络到 GitHub 下载域受限。可尝试:\n")
		fmt.Fprintf(os.Stderr, "  1. 设置镜像后重试: CLAUDE_PROXY_DOWNLOAD_MIRROR=<前缀> claude-proxy update\n")
		fmt.Fprintf(os.Stderr, "     (例如某些 GitHub 加速服务, 前缀会拼在原始 URL 前)\n")
		fmt.Fprintf(os.Stderr, "  2. 手动下载后替换: %s\n", assetURL)
		return 1
	}

	// 校验 sha256(从 checksums.txt)
	if sumURL := findAsset(rel, "checksums.txt"); sumURL != "" {
		if sums, err := downloadAsset(sumURL); err == nil {
			if !verifyChecksum(archive, assetName, sums) {
				fmt.Fprintln(os.Stderr, "sha256 校验失败,已中止(产物可能损坏或被篡改)")
				return 1
			}
			fmt.Println("sha256 校验通过。")
		}
	}

	// 从压缩包取出二进制
	binName := "claude-proxy"
	if runtime.GOOS == "windows" {
		binName = "claude-proxy.exe"
	}
	newBin, err := extractBinary(archive, assetName, binName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "解压失败: %v\n", err)
		return 1
	}

	if err := replaceSelf(newBin); err != nil {
		fmt.Fprintf(os.Stderr, "替换自身失败: %v\n", err)
		return 1
	}
	fmt.Printf("已更新到 %s。\n", latest)
	return 0
}

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func fetchLatestRelease() (*ghRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", repoOwner, repoName)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "claude-proxy-updater")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub API 返回 %d", resp.StatusCode)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func platformAssetName(tag string) string {
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("claude-proxy-%s-%s-%s.%s", tag, runtime.GOOS, runtime.GOARCH, ext)
}

func findAsset(rel *ghRelease, name string) string {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a.BrowserDownloadURL
		}
	}
	return ""
}

// downloadAsset 下载 release 资产。若设了 CLAUDE_PROXY_DOWNLOAD_MIRROR,
// 优先用镜像(前缀拼在原始 URL 前),失败再回退直连。
func downloadAsset(url string) ([]byte, error) {
	if mirror := os.Getenv("CLAUDE_PROXY_DOWNLOAD_MIRROR"); mirror != "" {
		mirrorURL := strings.TrimRight(mirror, "/") + "/" + url
		if data, err := download(mirrorURL); err == nil {
			return data, nil
		}
		// 镜像失败,回退直连
	}
	return download(url)
}

func download(url string) ([]byte, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "claude-proxy-updater")
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("下载返回 %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func verifyChecksum(archive []byte, assetName string, sums []byte) bool {
	sum := sha256.Sum256(archive)
	got := hex.EncodeToString(sum[:])
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == assetName {
			return strings.EqualFold(fields[0], got)
		}
	}
	// checksums.txt 里没有该条目时,不阻断(视为无校验数据)
	return true
}

// extractBinary 从 tar.gz 或 zip 里取出名为 binName(或以之结尾)的可执行文件。
func extractBinary(archive []byte, assetName, binName string) ([]byte, error) {
	if strings.HasSuffix(assetName, ".zip") {
		return extractFromZip(archive, binName)
	}
	return extractFromTarGz(archive, binName)
}

func extractFromTarGz(archive []byte, binName string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if matchBinary(hdr.Name, binName) {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("压缩包内未找到可执行文件")
}

func extractFromZip(archive []byte, binName string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if matchBinary(f.Name, binName) {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("压缩包内未找到可执行文件")
}

// matchBinary:产物里二进制名形如 claude-proxy-linux-amd64[.exe],
// 以 base 名匹配(去掉路径),包含 "claude-proxy" 即认。
func matchBinary(name, binName string) bool {
	base := filepath.Base(name)
	return strings.HasPrefix(base, "claude-proxy")
}

// replaceSelf 用新二进制原子替换当前可执行文件。
// 同目录写临时文件 -> chmod -> rename;Windows 上先把旧文件挪走。
func replaceSelf(newBin []byte) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, _ = filepath.EvalSymlinks(self)
	dir := filepath.Dir(self)

	tmp, err := os.CreateTemp(dir, ".claude-proxy-new-*")
	if err != nil {
		return fmt.Errorf("无法在 %s 写入(权限?): %w", dir, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(newBin); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	tmp.Close()
	if err := os.Chmod(tmpName, 0o755); err != nil {
		os.Remove(tmpName)
		return err
	}

	if runtime.GOOS == "windows" {
		// 运行中的 exe 不能被覆盖:先把旧的改名,再放新的。
		old := self + ".old"
		os.Remove(old)
		if err := os.Rename(self, old); err != nil {
			os.Remove(tmpName)
			return err
		}
		if err := os.Rename(tmpName, self); err != nil {
			os.Rename(old, self) // 回滚
			return err
		}
		os.Remove(old) // 可能因占用失败,忽略
		return nil
	}

	// Unix:直接 rename 覆盖(原子,同分区)
	if err := os.Rename(tmpName, self); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// isNewer 简单语义版本比较:latest 是否比 current 新。
// 形如 v1.2.3;current 为 "dev" 时一律视为可更新。
func isNewer(latest, current string) bool {
	if current == "dev" {
		return true
	}
	lp := parseVer(latest)
	cp := parseVer(current)
	for i := 0; i < 3; i++ {
		if lp[i] != cp[i] {
			return lp[i] > cp[i]
		}
	}
	return false
}

func parseVer(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	// 去掉预发布/构建后缀
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.SplitN(v, ".", 3)
	var out [3]int
	for i := 0; i < len(parts) && i < 3; i++ {
		n := 0
		for _, c := range parts[i] {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out
}
