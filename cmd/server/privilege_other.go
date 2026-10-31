//go:build !linux

package main

// dropPrivileges 在非 Linux 平台（本地开发、Windows/macOS 桌面）不做任何事：
// 既没有 UID 10001 这个用户，也没有容器绑定挂载带来的属主问题。
func dropPrivileges(string) error { return nil }
