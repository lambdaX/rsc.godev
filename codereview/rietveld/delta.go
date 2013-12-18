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
	"fmt"
	"io"
	"sort"
	"strings"
)

type FileOp string

const (
	Added    FileOp = "A"
	Deleted  FileOp = "D"
	Modified FileOp = "M"
)

type FileDiff struct {
	Op   FileOp
	Path string
	Text []byte
}

type Delta interface {
	// Patch returns details about the file differences in this patch.
	Patch() ([]*FileDiff, error)

	// Base returns the old content for a file that is part of the set
	// obtained in Patch. It is an error to attempt to obtain the base
	// for a FileDiff that has Op set to Added.
	Base(filename string) (io.ReadCloser, error)

	// BaseURL returns a URL that may be used to obtain the old content
	// for files in the set returned by Patch.
	BaseURL() string

	// SendBases returns whether the base files from this patch should
	// be uploaded to the rietveld server.
	SendBases() bool
}

// sortPatch sorts a patch by filename, putting entries of shallower
// depth first in the list.
func sortPatch(patch []*FileDiff) {
	sort.Sort(patchSorter(patch))
}

type patchSorter []*FileDiff

func (p patchSorter) Len() int      { return len(p) }
func (p patchSorter) Swap(i, j int) { p[i], p[j] = p[j], p[i] }
func (p patchSorter) Less(i, j int) bool {
	if strings.Count(p[i].Path, "/") < strings.Count(p[j].Path, "/") {
		return true
	}
	return p[i].Path < p[j].Path
}

// matchHandler is a convenient interface to result of regexp matches
// with the FindAllSubmatchIndex function.
type matchHandler struct {
	output  []byte
	matches [][]int
}

// Bytes returns bytes with the content of submatch sub on match m.
func (h *matchHandler) Bytes(m, sub int) []byte {
	return h.output[h.matches[m][sub*2]:h.matches[m][sub*2+1]]
}

// String returns a string with the content of submatch sub on match m.
func (h *matchHandler) String(m, sub int) string {
	return string(h.Bytes(m, sub))
}

// BytesRange returns bytes with the content between startMatch end endMatch.
func (h *matchHandler) BytesRange(startMatch int, endMatch int) []byte {
	var i, j int
	i = h.matches[startMatch][0]
	if endMatch == len(h.matches) {
		j = len(h.output)
	} else {
		j = h.matches[endMatch][0]
	}
	return h.output[i:j]
}

// run runs cmd with the given args, and returns the standard output,
// and a *CommandError in case of errors. Note that output and status
// are still set, even in case of errors, so that the caller can
// decide whether to move forward or not.
func run(cmd string, args ...string) (output []byte, status int, err error) {
	return nil, 0, fmt.Errorf("gone")
}

type CommandError struct {
	Command []string
	Stderr  []byte
	Err     error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("command %v failed: %v", e.Command, e.Err)
}
