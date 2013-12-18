// goetveld - Go interface to the Rietveld core review server.
//
//   https://wiki.ubuntu.com/goetveld
//
// Copyright (c) 2011 Canonical Ltd.
//
// Written by Gustavo Niemeyer <gustavo.niemeyer@canonical.com>
//
// This software is licensed under the GNU Lesser General Public License
// version 3 (LGPLv3), with an additional exception relative to static
// linkage. See the LICENSE file for details.

package rietveld

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"path/filepath"
	"regexp"
	"strings"
)

var logRevId = []byte("\nrevision-id: ")

// BazaarDiffBranches returns a Delta between the bazaar branch at
// oldPath and the one at newPath.
func BazaarDiffBranches(oldPath, newPath string) (Delta, error) {
	output1, _, err := run("bzr", "log", "-l1", "--show-ids", "-r", "ancestor:"+oldPath, newPath)
	if err != nil {
		return nil, err
	}
	output2, _, err := run("bzr", "log", "-l1", "--show-ids", newPath)
	if err != nil {
		return nil, err
	}
	i1 := bytes.Index(output1, logRevId)
	i2 := bytes.Index(output2, logRevId)
	if i1 < 0 || i2 < 0 {
		return nil, errors.New("no revision-id in bzr log output")
	}
	output1 = output1[i1+len(logRevId):]
	output2 = output2[i2+len(logRevId):]
	i1 = bytes.Index(output1, []byte{'\n'})
	i2 = bytes.Index(output2, []byte{'\n'})
	if i1 < 0 || i2 < 0 {
		return nil, errors.New("bad revision-id in bzr log output")
	}
	oldRevision := string(output1[:i1])
	newRevision := string(output2[:i2])
	return &bzrBranches{oldPath, newPath, oldRevision, newRevision}, nil
}

type bzrBranches struct {
	oldPath     string
	newPath     string
	oldRevision string
	newRevision string
}

var bzrRe = regexp.MustCompile(`(?m)^=== (added|removed|renamed|modified) (file|directory) (?:'.*' => )?'(.*)'(?: \(prop.*)?$`)

var bzrRevInfo = `=== added file '[revision details]'
--- [revision details]	2012-01-01 00:00:00 +0000
+++ [revision details]	2012-01-01 00:00:00 +0000
@@ -0,0 +1,2 @@
+Old revision: %s
+New revision: %s
`

func (b *bzrBranches) Patch() (patch []*FileDiff, err error) {
	revisions := fmt.Sprint("revid:", b.oldRevision, "..revid:", b.newRevision, "")
	output, status, err := run("bzr", "diff", "-r", revisions, b.newPath)
	// Status 1 just means there are changes.
	if err != nil && status != 1 {
		return nil, err
	}
	matches := bzrRe.FindAllSubmatchIndex(output, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("diff is empty")
	}

	diff := &FileDiff{
		Path: "[revision details]",
		Op:   Added,
		Text: []byte(fmt.Sprintf(bzrRevInfo, b.oldRevision, b.newRevision)),
	}
	patch = append(patch, diff)

	h := &matchHandler{output, matches}
	for i := range matches {
		if h.String(i, 2) == "directory" {
			continue
		}
		diff := &FileDiff{Path: h.String(i, 3)}
		switch h.String(i, 1) {
		case "added":
			diff.Op = Added
		case "removed":
			diff.Op = Deleted
		case "renamed", "modified":
			diff.Op = Modified
		default:
			panic("unreachable")
		}
		diff.Text = h.BytesRange(i, i+1)
		patch = append(patch, diff)
	}
	sortPatch(patch)
	return patch, nil
}

func (b *bzrBranches) Base(filename string) (io.ReadCloser, error) {
	output, _, err := run("bzr", "cat", "-r", "revid:"+b.oldRevision, filepath.Join(b.newPath, filename))
	if err != nil {
		return nil, err
	}
	return ioutil.NopCloser(bytes.NewBuffer(output)), nil
}

func quote(s string) string {
	return "'" + strings.Replace(s, "'", `'\''`, -1) + "'"
}

func (b *bzrBranches) BaseURL() string {
	return ""
}

func (b *bzrBranches) SendBases() bool {
	return true
}
