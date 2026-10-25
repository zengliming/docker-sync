// Package remote copies archives to SMB shares without privileged mounts.
//
// 传输层使用纯 Go 的 SMB2/SMB3 客户端，不依赖 smbclient 或内核 cifs 挂载，
// 所以容器里不用装 samba-client，Windows / macOS 宿主机也不需要额外组件。
package remote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/hirochachacha/go-smb2"

	"docker-sync/internal/model"
)

const (
	// smbPort 是 SMB 服务端监听端口，几乎总是 445。
	smbPort = "445"
	// chunkSize 决定单次写入大小：太大不利于及时响应取消，太小会拖慢吞吐。
	chunkSize = 1 << 20
	// dialTimeout 覆盖 TCP 连接加 SMB 协商的最长耗时。
	dialTimeout = 30 * time.Second
)

var sharePattern = regexp.MustCompile(`^//[a-zA-Z0-9_.:-]+/[a-zA-Z0-9_.$ -]+$`)

// Validate rejects malformed settings and injection-prone values.
func Validate(c model.SMBSettings) error {
	if !c.Enabled {
		return nil
	}
	if !sharePattern.MatchString(c.Share) {
		return fmt.Errorf("smb.share 必须是 //服务器/共享名，且不能带子目录")
	}
	for _, v := range []string{c.Username, c.Password, c.Domain} {
		if strings.ContainsAny(v, "\r\n\x00") || strings.TrimSpace(v) != v {
			return fmt.Errorf("SMB 用户名、密码、域不能包含换行、NUL 或首尾空白")
		}
	}
	if c.Username == "" {
		return fmt.Errorf("smb.username 必填")
	}
	if strings.ContainsFunc(c.Directory, unicode.IsControl) {
		return fmt.Errorf("smb.directory 不能包含控制字符")
	}
	if strings.HasPrefix(c.Directory, "/") || strings.ContainsAny(c.Directory, "\\\";\r\n\x00") {
		return fmt.Errorf("smb.directory 必须是相对路径，且不能包含引号、反斜杠或控制字符")
	}
	for _, part := range strings.Split(c.Directory, "/") {
		if part == ".." {
			return fmt.Errorf("smb.directory 不能包含 ..")
		}
	}
	return nil
}

// ProgressFunc 报告已写入远端的字节数，用于日志或界面展示。
type ProgressFunc func(copied, total int64)

// Upload publishes a complete file to the configured share.
//
// 远端先写成唯一的临时名，成功后再重命名，避免对端读到半截 tar；本地归档永远保留。
func Upload(ctx context.Context, cfg model.SMBSettings, path string) error {
	return upload(ctx, cfg, path, dialShare, nil)
}

// UploadWithProgress 与 Upload 相同，但会额外汇报进度。
func UploadWithProgress(ctx context.Context, cfg model.SMBSettings, path string, progress ProgressFunc) error {
	return upload(ctx, cfg, path, dialShare, progress)
}

// shareSession 是上传所需的最小远端接口，便于用假实现做单元测试。
type shareSession interface {
	Create(name string) (io.WriteCloser, error)
	Rename(oldName, newName string) error
	Remove(name string) error
	Close() error
}

type dialFunc func(ctx context.Context, cfg model.SMBSettings) (shareSession, error)

func upload(ctx context.Context, cfg model.SMBSettings, path string, dial dialFunc, progress ProgressFunc) error {
	if !cfg.Enabled {
		return nil
	}
	if err := Validate(cfg); err != nil {
		return err
	}
	name := filepath.Base(path)
	if strings.ContainsAny(name, "\";\\\r\n\x00") || !strings.HasSuffix(name, ".tar") {
		return fmt.Errorf("归档文件名不合法: %q", name)
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("归档不是普通文件: %s", path)
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()

	partial, err := partialName()
	if err != nil {
		return err
	}

	sess, err := dial(ctx, cfg)
	if err != nil {
		return fmt.Errorf("SMB 上传失败: %w", err)
	}
	defer sess.Close()

	dst, err := sess.Create(partial)
	if err != nil {
		return fmt.Errorf("SMB 上传失败: 无法在共享 %s 中创建临时文件（检查目录是否存在、账号是否有写权限）: %w", cfg.Share, err)
	}
	copied, copyErr := copyChunks(ctx, dst, src, info.Size(), progress)
	closeErr := dst.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		discard(ctx, sess, partial)
		return fmt.Errorf("SMB 上传失败: 写入 %s 在 %d/%d 字节处中断: %w", name, copied, info.Size(), copyErr)
	}
	// 重命名不是原子覆盖：先删同名的旧文件，失败（通常是不存在）忽略。
	_ = sess.Remove(name)
	if err := sess.Rename(partial, name); err != nil {
		discard(ctx, sess, partial)
		return fmt.Errorf("SMB 上传失败: 重命名 %s 失败: %w", name, err)
	}
	return nil
}

// discard 尽力清理远端临时文件。
func discard(ctx context.Context, sess shareSession, name string) {
	if ctx.Err() != nil {
		// 上下文已取消，连接大概率不可用，跳过清理。
		return
	}
	_ = sess.Remove(name)
}

func copyChunks(ctx context.Context, dst io.Writer, src io.Reader, total int64, progress ProgressFunc) (int64, error) {
	buf := make([]byte, chunkSize)
	var copied int64
	for {
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return copied, writeErr
			}
			copied += int64(n)
			if progress != nil {
				progress(copied, total)
			}
		}
		if readErr == io.EOF {
			return copied, nil
		}
		if readErr != nil {
			return copied, readErr
		}
	}
}

func partialName() (string, error) {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return ".docker-sync-" + hex.EncodeToString(id[:]) + ".part", nil
}

// splitShare 把 //server/share 拆成主机名与共享名。
func splitShare(share string) (host, name string, err error) {
	parts := strings.SplitN(strings.TrimPrefix(share, "//"), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("smb.share %q 无法解析，应为 //服务器/共享名", share)
	}
	return parts[0], parts[1], nil
}

// remoteDir 把配置里的相对目录转成 SMB 使用的反斜杠形式。
func remoteDir(dir string) string {
	dir = strings.Trim(strings.TrimSpace(dir), `/\`)
	if dir == "" {
		return ""
	}
	return strings.ReplaceAll(dir, "/", `\`)
}

func joinRemote(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + `\` + name
}

// smbShareSession 是 shareSession 在真实 SMB 连接上的实现。
type smbShareSession struct {
	conn    net.Conn
	session *smb2.Session
	fs      *smb2.Share
	dir     string
}

func (s *smbShareSession) Create(name string) (io.WriteCloser, error) {
	return s.fs.Create(joinRemote(s.dir, name))
}

func (s *smbShareSession) Rename(oldName, newName string) error {
	return s.fs.Rename(joinRemote(s.dir, oldName), joinRemote(s.dir, newName))
}

func (s *smbShareSession) Remove(name string) error {
	return s.fs.Remove(joinRemote(s.dir, name))
}

func (s *smbShareSession) Close() error {
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.fs != nil {
		keep(s.fs.Umount())
	}
	if s.session != nil {
		keep(s.session.Logoff())
	}
	if s.conn != nil {
		keep(s.conn.Close())
	}
	return firstErr
}

// hostPort returns host:445 unless the share already pins an explicit port.
func hostPort(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	// 裸 IPv6 字面量需要去掉方括号，JoinHostPort 会重新加上。
	return net.JoinHostPort(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"), smbPort)
}

// dialShare 用纯 Go 的 SMB2/3 客户端连接共享，不依赖 smbclient。
func dialShare(ctx context.Context, cfg model.SMBSettings) (shareSession, error) {
	host, share, err := splitShare(cfg.Share)
	if err != nil {
		return nil, err
	}
	addr := hostPort(host)
	dialer := &net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("连接 %s 失败（需要能访问 %s 端口）: %w", host, smbPort, err)
	}
	d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{
		User:     cfg.Username,
		Password: cfg.Password,
		Domain:   cfg.Domain,
	}}
	session, err := d.DialContext(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("SMB 登录 %s 失败（检查用户名、密码、域）: %w", host, err)
	}
	fs, err := session.Mount(`\\` + host + `\` + share)
	if err != nil {
		_ = session.Logoff()
		_ = conn.Close()
		return nil, fmt.Errorf("打开共享 %s 失败（共享名是否正确、账号是否有权限）: %w", cfg.Share, err)
	}
	return &smbShareSession{conn: conn, session: session, fs: fs.WithContext(ctx), dir: remoteDir(cfg.Directory)}, nil
}
