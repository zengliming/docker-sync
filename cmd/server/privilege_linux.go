//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const (
	// 镜像内服务用户的 UID/GID，与 Dockerfile 中创建的一致。
	serviceUID = 10001
	serviceGID = 10001
)

// dropPrivileges 在容器以 root 启动时接管数据目录并降权到服务用户。
//
// 绑定挂载（./data:/data）的属主由宿主机决定，若不属于服务用户，生成 tar
// 时会 permission denied。镜像刻意不依赖 su-exec/gosu 这类外部工具，而是在
// 进程内完成 chown + setuid，这样精简镜像和手工拼装的镜像都能正常工作。
//
// 必须在打开任何状态文件、启动任何 goroutine 之前调用：Go 的 syscall.Setuid
// 会同步到所有线程，切换后进程内的文件操作都以服务用户身份执行。
func dropPrivileges(dataDir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录 %s 失败: %w", dataDir, err)
	}
	chownTree(dataDir, serviceUID, serviceGID)
	if err := syscall.Setgid(serviceGID); err != nil {
		return fmt.Errorf("切换服务用户组失败: %w", err)
	}
	if err := syscall.Setuid(serviceUID); err != nil {
		return fmt.Errorf("切换服务用户失败: %w", err)
	}
	probe, err := os.CreateTemp(dataDir, ".write-test-*")
	if err != nil {
		return fmt.Errorf("数据目录 %s 对 UID %d 不可写，请在宿主机执行 chown -R %d:%d <数据目录>；NFS/SMB 挂载或启用 userns-remap 时需改用命名卷: %w",
			dataDir, serviceUID, serviceUID, serviceGID, err)
	}
	name := probe.Name()
	probe.Close()
	os.Remove(name)
	return nil
}

// chownTree 递归修正属主。个别条目失败（只读挂载、无权限的子目录）不影响
// 其余条目，最终能否写入由调用方创建探针文件来判断。
func chownTree(root string, uid, gid int) {
	_ = filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		_ = os.Chown(path, uid, gid)
		return nil
	})
}
