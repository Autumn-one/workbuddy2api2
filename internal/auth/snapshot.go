// snapshot.go — 账号凭证目录的时间戳快照备份。
//
// 背景：本项目的账号均为一次性登录凭证，auths/*.json 一旦丢失/损坏即不可恢复。
// 除原子写防半文件外，这里提供整目录快照：内容有变化时落一份带时间戳的副本，
// 内容不变则跳过（指纹去重），并按 keep 修剪最旧快照。
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SnapshotDir 把 authDir 下所有 workbuddy*.json 复制到
// <authDir 同级>/auths-backup/auths-<yyyymmdd-HHMMSS>/。
//
// 返回快照目录与 changed（是否真的写了新快照）：
//   - 目录为空（无凭证文件）→ ("", false, nil)，不做任何备份；
//   - 内容与最近一次快照一致 → (最近快照路径, false, nil)，不重复落盘；
//   - 有变化 → (新快照路径, true, nil)。
//
// 指纹存于每份快照内的 .fingerprint 文件（sha256(文件名+内容) 拼接），
// 比对只读最近一份快照的指纹，开销可忽略，允许每写必调。
func SnapshotDir(authDir string, keep int) (snapPath string, changed bool, err error) {
	files, err := filepath.Glob(filepath.Join(authDir, "workbuddy*.json"))
	if err != nil {
		return "", false, err
	}
	if len(files) == 0 {
		return "", false, nil
	}
	type blob struct {
		name string
		raw  []byte
	}
	var blobs []blob
	h := sha256.New()
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue // 读不了的文件不进快照也不影响其它文件备份
		}
		name := filepath.Base(f)
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(raw)
		h.Write([]byte{0})
		blobs = append(blobs, blob{name: name, raw: raw})
	}
	if len(blobs) == 0 {
		return "", false, nil
	}
	fp := hex.EncodeToString(h.Sum(nil))

	root := filepath.Join(filepath.Dir(authDir), "auths-backup")
	snaps, _ := snapshotList(root)
	if n := len(snaps); n > 0 {
		latest := filepath.Join(root, snaps[n-1])
		if b, err := os.ReadFile(filepath.Join(latest, ".fingerprint")); err == nil &&
			strings.TrimSpace(string(b)) == fp {
			return latest, false, nil
		}
	}

	dir := filepath.Join(root, "auths-"+time.Now().Format("20060102-150405"))
	// 同秒撞名（两次快照间隔 <1s 且内容不同）：追加序号后缀避让。
	for i := 2; ; i++ {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			break
		}
		dir = filepath.Join(root, "auths-"+time.Now().Format("20060102-150405")+"-"+strconv.Itoa(i))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, err
	}
	for _, b := range blobs {
		if err := WriteFileAtomic(filepath.Join(dir, b.name), b.raw, 0o600); err != nil {
			return dir, true, err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".fingerprint"), []byte(fp), 0o600); err != nil {
		return dir, true, err
	}

	if keep > 0 {
		snaps, _ = snapshotList(root)
		for len(snaps) > keep {
			_ = os.RemoveAll(filepath.Join(root, snaps[0]))
			snaps = snaps[1:]
		}
	}
	return dir, true, nil
}

// snapshotList 返回 root 下所有 auths-* 快照目录名（按名字排序 = 按时间排序）。
func snapshotList(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "auths-") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
