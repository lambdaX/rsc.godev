package rietveld_test

import (
	"code.google.com/p/rsc/codebot/rietveld"
	"fmt"
	"io/ioutil"
	. "launchpad.net/gocheck"
	"os/exec"
	"path/filepath"
)

func init() {
	Suite(&BazaarS{})
}

type BazaarS struct{}

func run(format string, args ...interface{}) (output string) {
	cmd := fmt.Sprintf(format, args...)
	out, err := exec.Command("/bin/sh", "-c", cmd).CombinedOutput()
	if err != nil {
		panic(fmt.Errorf("error running '%s': %s\noutput:\n%s\n", cmd, err, out))
	}
	return string(out)
}

var bazaarBranchesSetup = `
set -e

mkdir b1
cd b1
mkdir dir
echo A > file1\'\$bug
echo X > dir/file3
bzr init
bzr add .
bzr commit -m 1
cd ..

bzr branch b1 b2
cd b2
chmod +x file1\'\$bug
echo B > file1\'\$bug
echo C > file2
echo Y > dir/file3
bzr add file2
bzr commit -m 2
cd ..

echo changed > b1/file1
`

type wantDiff struct {
	Op     rietveld.FileOp
	Path   string
	Base   string
	Regexp string
}

var bazaarBranchesPatch = []wantDiff{
	{
		rietveld.Added, `[revision details]`, "A\n",
		`=== added file '\[revision details\]'\n` +
			`--- \[revision details\]\t2012-01-01.*\n` +
			`\+\+\+ \[revision details\]\t2012-01-01.*\n` +
			`@@ -0,0 \+1,2 @@\n` +
			`\+Old revision: [a-zA-Z0-9@+=._-]+\n` +
			`\+New revision: [a-zA-Z0-9@+=._-]+\n`,
	},
	{
		rietveld.Modified, "file1'$bug", "A\n",
		`=== modified file 'file1'\$bug' \(properties changed: -x to \+x\)\n` +
			`--- file1'\$bug\t.*\n` +
			`\+\+\+ file1'\$bug\t.*\n` +
			`@@ -1,1 \+1,1 @@\n` +
			`-A\n` +
			`\+B\n\n`,
	},
	{
		rietveld.Added, "file2", "",
		`=== added file 'file2'\n` +
			`--- file2\t.*\n` +
			`\+\+\+ file2\t.*\n` +
			`@@ -0,0 \+1,1 @@\n` +
			`\+C\n\n`,
	},
	{
		rietveld.Modified, "dir/file3", "X\n",
		`=== modified file 'dir/file3'\n` +
			`--- dir/file3\t.*\n` +
			`\+\+\+ dir/file3\t.*\n` +
			`@@ -1,1 \+1,1 @@\n` +
			`-X\n` +
			`\+Y\n\n`,
	},
}

func (s *BazaarS) TestBazaarDiffBranches(c *C) {
	tmpdir := c.MkDir()
	err := ioutil.WriteFile(filepath.Join(tmpdir, "setup.sh"), []byte(bazaarBranchesSetup), 0755)
	c.Assert(err, IsNil)

	run("cd %s; ./setup.sh", tmpdir)

	b1 := filepath.Join(tmpdir, "b1")
	b2 := filepath.Join(tmpdir, "b2")

	delta, err := rietveld.BazaarDiffBranches(b1, b2)
	c.Assert(err, IsNil)
	c.Assert(delta.BaseURL(), Equals, "")
	c.Assert(delta.SendBases(), Equals, true)

	patch, err := delta.Patch()
	c.Assert(err, IsNil)

	want := bazaarBranchesPatch
	for i := range want {
		if i == len(patch) {
			c.Fatalf("missing diff for file %q", want[i].Path)
		}
		c.Assert(patch[i].Op, Equals, want[i].Op)
		c.Assert(patch[i].Path, Equals, want[i].Path)
		c.Assert(string(patch[i].Text), Matches, want[i].Regexp)

		if patch[i].Op == rietveld.Added {
			continue
		}
		rc, err := delta.Base(patch[i].Path)
		c.Assert(err, IsNil)
		base, err := ioutil.ReadAll(rc)
		rc.Close()
		c.Assert(err, IsNil)
		c.Assert(string(base), Equals, want[i].Base)
	}
	if len(want) < len(patch) {
		c.Fatalf("got unexpected file %q in patch", patch[len(want)].Path)
	}
}
