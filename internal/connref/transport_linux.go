//go:build linux

package connref

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const maxSource = 16384

// Walk by directory descriptors, without reopening a validated pathname.
// Root-owned sticky directories (e.g. /tmp) cannot replace our owned child.
func readProtected(path string) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errReference
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errReference
	}
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			unix.Close(fd)
			return nil, errReference
		}
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, e := unix.Openat(fd, part, flags, 0)
		unix.Close(fd)
		if e != nil {
			return nil, errReference
		}
		fd = next
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil {
			unix.Close(fd)
			return nil, errReference
		}
		if i < len(parts)-1 {
			trustedOwner := st.Uid == 0 || st.Uid == uint32(os.Geteuid())
			safeMode := st.Mode&0022 == 0 || (st.Uid == 0 && st.Mode&unix.S_ISVTX != 0)
			if !trustedOwner || !safeMode {
				unix.Close(fd)
				return nil, errReference
			}
		}
	}
	defer unix.Close(fd)
	var before, after unix.Stat_t
	if unix.Fstat(fd, &before) != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Uid != uint32(os.Geteuid()) || (before.Mode&07777 != 0600 && before.Mode&07777 != 0400) || before.Nlink != 1 || before.Size <= 0 || before.Size > maxSource {
		return nil, errReference
	}
	body := make([]byte, maxSource+1)
	n := 0
	for n < len(body) {
		count, e := unix.Read(fd, body[n:])
		if e == unix.EINTR {
			continue
		}
		if e != nil {
			return nil, errReference
		}
		n += count
		if count == 0 {
			break
		}
	}
	if n > maxSource || unix.Fstat(fd, &after) != nil || before.Size != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim {
		return nil, errReference
	}
	return body[:n], nil
}

func parseMaterial(dsn string) (*material, error) { return parseWithPassword(dsn, nil) }

func parsePasswordMaterial(dsn, password string) (*material, error) {
	return parseWithPassword(dsn, &password)
}

func parseWithPassword(dsn string, supplied *string) (*material, error) {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.User == nil || u.Fragment != "" {
		return nil, errReference
	}
	password, present := u.User.Password()
	if supplied != nil {
		if present {
			return nil, errReference
		}
		password = *supplied
	} else if !present {
		return nil, errReference
	}
	host, user, db := u.Hostname(), u.User.Username(), strings.TrimPrefix(u.Path, "/")
	if !validField(host) || strings.Contains(host, ",") || !validField(user) || !validField(db) || len(db) > 63 || strings.ContainsAny(password, "\x00\r\n") {
		return nil, errReference
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 1 || n > 65535 {
		return nil, errReference
	}
	q, e := url.ParseQuery(u.RawQuery)
	if e != nil {
		return nil, errReference
	}
	for key, values := range q {
		if len(values) != 1 {
			return nil, errReference
		}
		switch key {
		case "sslmode":
			switch values[0] {
			case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
			default:
				return nil, errReference
			}
		case "connect_timeout":
			timeout, e := strconv.Atoi(values[0])
			if e != nil || timeout < 1 || timeout > 120 {
				return nil, errReference
			}
		case "application_name":
			// Only versioned caller profiles may select metadata copied into a
			// returned DSN. An arbitrary value could itself be a credential.
			switch values[0] {
			case "fabd", "fabd-install", "fab-tui":
			default:
				return nil, errReference
			}
		default:
			return nil, errReference
		}
	}
	u.User = url.User(user)
	u.Host = net.JoinHostPort(host, port)
	u.RawQuery = q.Encode()
	u.RawPath = ""
	u.ForceQuery = false
	return &material{base: *u, password: password}, nil
}

func pgpassEscape(s string) string { return strings.NewReplacer("\\", "\\\\", ":", "\\:").Replace(s) }

func makeRef(m *material, db string) (*Ref, error) {
	if !validField(db) || len(db) > 63 {
		return nil, errReference
	}
	fd, err := unix.MemfdCreate("fabric-db-ref", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, errReference
	}
	f := os.NewFile(uintptr(fd), "fabric-db-ref")
	good := false
	defer func() {
		if !good {
			f.Close()
		}
	}()
	record := fmt.Sprintf("%s:%s:%s:%s:%s\n", pgpassEscape(m.base.Hostname()), m.base.Port(), pgpassEscape(db), pgpassEscape(m.base.User.Username()), pgpassEscape(m.password))
	if unix.Fchmod(fd, 0600) != nil {
		return nil, errReference
	}
	if _, err = f.Write([]byte(record)); err != nil {
		return nil, errReference
	}
	if _, err = unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
		return nil, errReference
	}
	u := m.base
	u.Path = "/" + db
	u.RawPath = ""
	q := u.Query()
	q.Set("passfile", fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), fd))
	u.RawQuery = q.Encode()
	m.owners.Add(1)
	good = true
	return &Ref{dsn: u.String(), file: f, material: m, purpose: m.purpose}, nil
}
