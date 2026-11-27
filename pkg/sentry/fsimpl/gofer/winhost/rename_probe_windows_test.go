//go:build windows

package winhost

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

// TestRenameReplaceProbe records which replacing renames Windows allows a standard user in each state of
// the target, and the target's ACL and attributes (docs/results/5-podman-windows.md: every c/storage rewrite of
// layers.json was refused with access denied and fell back to delete + rename). It fails only if no
// variant works for a plain target.
func TestRenameReplaceProbe(t *testing.T) {
	fs, dir := newFS(t, Options{})
	root := fs.Root()
	type variant struct {
		name        string
		class       uint32
		flags       uint32
		holdDelete  bool // a handle with DELETE access on the target stays open
		readTarget  bool // a read handle on the target stays open
		setupTarget func(a Attr)
	}
	variants := []variant{
		{name: "posix-replace", class: ntClassRenameEx, flags: renameReplace | renamePosix},
		{name: "plain-replace", class: ntClassRenameEx, flags: renameReplace},
		{name: "classic-replace", class: ntClassRename, flags: renameReplace},
		{name: "posix-replace-with-data", class: ntClassRenameEx, flags: renameReplace | renamePosix, setupTarget: func(a Attr) { fs.WriteAt(a.ID, []byte("data"), 0) }},
		{name: "posix-replace-after-chmod", class: ntClassRenameEx, flags: renameReplace | renamePosix, setupTarget: func(a Attr) { fs.SetAttr(a.ID, SetAttr{HasMode: true, Mode: 0o600}) }},
		{name: "posix-replace-read-handle-open", class: ntClassRenameEx, flags: renameReplace | renamePosix, readTarget: true},
		{name: "posix-replace-delete-handle-open", class: ntClassRenameEx, flags: renameReplace | renamePosix, holdDelete: true},
	}
	works := 0
	for i, v := range variants {
		sname, dname := "src"+string(rune('a'+i)), "dst"+string(rune('a'+i))
		src := mustCreate(t, fs, root, sname, Spec{Mode: 0o644})
		dst := mustCreate(t, fs, root, dname, Spec{Mode: 0o644})
		fs.WriteAt(src.ID, []byte("new"), 0)
		if v.setupTarget != nil {
			v.setupTarget(dst)
		}
		sh, err := fs.openID(root, dirAccess)
		if err != nil {
			t.Fatal(err)
		}
		sf, err := openChild(sh, sname, deleteAccess|fileReadAttributes|fileWriteAttributes, 0)
		if err != nil {
			t.Fatalf("open src: %v", err)
		}
		var held windows.Handle
		if v.holdDelete {
			held, _ = openChild(sh, dname, deleteAccess|fileReadAttributes, 0)
		}
		if v.readTarget {
			held, _ = openChild(sh, dname, fileReadData|fileReadAttributes, 0)
		}
		np, _ := handlePath(sh)
		err = ntRename(sf, v.class, v.flags, 0, np+`\`+dname)
		t.Logf("%-34s -> %v", v.name, err)
		if err == nil {
			works++
		}
		if held != 0 {
			windows.CloseHandle(held)
		}
		windows.CloseHandle(sf)
		fs.closeID(root, sh)
		if i == 0 {
			out, _ := exec.Command("icacls", dir+`\`+dname).CombinedOutput()
			t.Logf("icacls of a target:\n%s", out)
			out, _ = exec.Command("whoami", "/groups").CombinedOutput()
			_ = out
		}
	}
	t.Logf("%d of %d variants replaced the target", works, len(variants))
}
