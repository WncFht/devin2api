package config

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic 经临时文件 + Sync + Rename 原子替换目标文件：崩溃
// 窗只会留下无名 .tmp，读者要么看到旧版完整内容要么是新版，不会拿到
// 半截文件（小状态件的损坏面集中在 write 窗口，rename 语义把它收成
// 空）。临时文件与目标同目录保证 rename 不跨文件系统；调用方负责目录
// 存在。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	// Sync 必须在 Close 前：掉电时页缓存可能还没落盘，rename 之后
	// 目标名会指向一个内容从未持久化的 inode——文件存在但为零长。
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
