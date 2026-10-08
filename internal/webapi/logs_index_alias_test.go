package webapi

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/wlame/rx-go/internal/tasks"
)

// aliasCases are two handles of one chain each: the chain app.log in
// root/logs, named once through the directory as it is written and once
// through another name of the same directory.
var aliasCases = []struct {
	name string
	// alias makes another name of dir inside root and returns it, or
	// skips the test when the disk has none.
	alias func(t *testing.T, root, dir string) string
}{
	{"another case on a case-insensitive disk", func(t *testing.T, root, _ string) string {
		upper := filepath.Join(root, "LOGS")
		if _, err := os.Stat(upper); err != nil {
			t.Skip("the test disk is case-sensitive: LOGS is not logs")
		}
		return upper
	}},
	{"a symbolic link to the directory", func(t *testing.T, root, dir string) string {
		link := filepath.Join(root, "link")
		if err := os.Symlink(dir, link); err != nil {
			t.Fatal(err)
		}
		return link
	}},
}

// Two handles of one chain, through two names of its directory, share
// the chain's one index task: a describe and a POST through the alias
// join the task the first handle started. The task shows the handle it
// was started with; index_build shows each request's own handle.
func TestChainIndex_AnAliasOfTheDirectoryJoinsTheChainsTask(t *testing.T) {
	for _, tc := range aliasCases {
		t.Run(tc.name, func(t *testing.T) {
			f, gate := newGatedChainFixture(t, 2)
			dir := filepath.Join(f.root, "logs")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			writeIndexChain(t, dir, threePartChain, 10, chainStart)
			handle := filepath.Join(dir, "app.log")
			aliasHandle := filepath.Join(tc.alias(t, f.root, dir), "app.log")

			first := describeChainAt(t, f.base, handle).IndexBuild
			gate.awaitStart(t)
			alias := describeChainAt(t, f.base, aliasHandle).IndexBuild
			posted := postTask(t, f.base, url.Values{"path": {aliasHandle}})

			if alias == nil || alias.TaskID != first.TaskID || alias.Path != aliasHandle {
				t.Fatalf("describe through the alias: index_build %+v, want task %s shown as %s", alias, first.TaskID, aliasHandle)
			}
			if posted.TaskID != first.TaskID {
				t.Fatalf("POST through the alias: task %s, want the running %s", posted.TaskID, first.TaskID)
			}
			if task, _ := f.manager.Get(first.TaskID); task.Path != handle {
				t.Fatalf("the task shows %s, want the handle it was started with, %s", task.Path, handle)
			}
			gate.open()
			if task := awaitTaskEnd(t, f.manager, first.TaskID); task.Status != tasks.StatusCompleted {
				t.Fatalf("task %s: %s", task.Status, task.Error)
			}
		})
	}
}
