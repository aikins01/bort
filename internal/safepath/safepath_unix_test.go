//go:build !windows

package safepath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOpenExistingPrivateDirNoFollowAcceptsOnlyProtectedWritableAncestors(t *testing.T) {
	for _, test := range []struct {
		name    string
		mode    os.FileMode
		wantErr string
	}{
		{name: "sticky", mode: 0o0777 | os.ModeSticky},
		{name: "group-writable", mode: 0o0720, wantErr: "writable by group or others"},
		{name: "other-writable", mode: 0o0702, wantErr: "writable by group or others"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ancestor := filepath.Join(root, "writable")
			if err := os.Mkdir(ancestor, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(ancestor, test.mode); err != nil {
				t.Fatal(err)
			}
			private := filepath.Join(ancestor, "private")
			if err := os.Mkdir(private, 0o700); err != nil {
				t.Fatal(err)
			}

			dir, err := OpenExistingPrivateDirNoFollow(private)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("open private directory below sticky ancestor: %v", err)
				}
				if dir == nil {
					t.Fatal("open private directory below sticky ancestor returned nil")
				}
				if err := dir.Close(); err != nil {
					t.Fatal(err)
				}
				return
			}
			if dir != nil {
				_ = dir.Close()
				t.Fatal("opened private directory below non-sticky writable ancestor")
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("open private directory below non-sticky writable ancestor: %v", err)
			}
		})
	}
}

func TestValidatePrivateDirMetadataRejectsUntrustedStickyOwner(t *testing.T) {
	t.Setenv("SUDO_UID", "")
	untrustedUID := uint32(1)
	if untrustedUID == uint32(os.Geteuid()) {
		untrustedUID = 2
	}
	err := validatePrivateDirMetadata(unix.S_ISVTX|0o0777, untrustedUID, "/sticky", true)
	if err == nil || !strings.Contains(err.Error(), "owned by untrusted uid") {
		t.Fatalf("validate untrusted sticky directory owner: %v", err)
	}
}

func TestPrivateDirValidatePathRejectsRelaxedPermissions(t *testing.T) {
	for _, test := range []struct {
		name string
		mode os.FileMode
	}{
		{name: "sticky-writable", mode: 0o0722 | os.ModeSticky},
		{name: "readable", mode: 0o0744},
		{name: "executable", mode: 0o0711},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			private := filepath.Join(root, "private")
			if err := os.Mkdir(private, 0o700); err != nil {
				t.Fatal(err)
			}
			dir, err := OpenExistingPrivateDirNoFollow(private)
			if err != nil {
				t.Fatal(err)
			}
			defer dir.Close()
			if err := os.Chmod(private, test.mode); err != nil {
				t.Fatal(err)
			}
			if err := dir.ValidatePath(); err == nil || !strings.Contains(err.Error(), "remove group and other access") {
				t.Fatalf("validate private directory with relaxed permissions: %v", err)
			}
		})
	}
}
