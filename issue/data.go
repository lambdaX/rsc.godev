// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package issue

import "time"

// An Issue represents a single issue on the tracker.
// The initial report is Comment[0] and is always present.
type Issue struct {
	DV         int `dataversion:"3"`
	ID         int
	Created    time.Time
	Modified   time.Time
	Summary    string
	Status     string
	Duplicate  int // if Status == "Duplicate"
	Owner      string
	CC         []string
	Label      []string
	Comment    []Comment `datastore:",noindex"`
	State      string
	Stars      int
	ClosedDate time.Time
}

// A Comment represents a single comment on an issue.
type Comment struct {
	Author    string
	Time      time.Time
	Summary   string
	Status    string
	Duplicate int // if Status == "Duplicate"
	Owner     string
	CC        string
	Label     string
	Text      string
}

type BySummary []*Issue

func (x BySummary) Len() int           { return len(x) }
func (x BySummary) Swap(i, j int)      { x[i], x[j] = x[j], x[i] }
func (x BySummary) Less(i, j int) bool { return x[i].Summary < x[j].Summary }

type ByID []*Issue

func (x ByID) Len() int           { return len(x) }
func (x ByID) Swap(i, j int)      { x[i], x[j] = x[j], x[i] }
func (x ByID) Less(i, j int) bool { return x[i].ID < x[j].ID }
