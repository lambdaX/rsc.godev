// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package codereview

import (
	"strings"
	"time"

	"app"
)

type CL struct {
	DV int `dataversion:"6"`

	// Fields mirrored from codereview.appspot.com.
	// If you add a field here, update load.go.
	CL         string
	Desc       string `datastore:",noindex"`
	Owner      string
	OwnerEmail string
	Created    time.Time
	Modified   time.Time
	Messages   []Message `datastore:",noindex"`
	Reviewers  []string
	CC         []string
	Closed     bool
	Submitted  bool
	PatchSets  []string

	// Derived fields.
	MessagesLoaded  bool   // Messages are up to date.
	PatchSetsLoaded bool   // PatchSets have been stored (separately).
	HasReviewers    bool   // len(Reviewers) > 0
	Summary         string // first line of Desc
	Active          bool   // HasReviewers && !Closed && !Submitted
}

func isSubmitted(cl *CL) bool {
	for _, m := range cl.Messages {
		if strings.Contains(m.Text, "*** Submitted as") {
			return true
		}
	}
	return false
}

func updateCL(cl *CL) {
	cl.HasReviewers = len(cl.Reviewers) > 0

	cl.Active = cl.HasReviewers && !cl.Closed &&
		cl.MessagesLoaded && len(cl.Messages) > 0 &&
		time.Since(cl.Modified) < 365*24*time.Hour

	for i, m := range cl.Messages {
		if strings.Contains(m.Text, "*** Submitted as") {
			cl.Active = false
			cl.Submitted = true
		}
		if i == len(cl.Messages)-1 && strings.Contains(m.Text, "Q=wait") {
			cl.Active = false
		}
		if strings.Contains(m.Text, "R=close") {
			cl.Active = false
		}
	}

	s := strings.TrimSpace(cl.Desc)
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 100 {
		s = s[:100]
	}
	cl.Summary = s
}

func init() {
	app.RegisterDataUpdater("CL", updateCL)
}

type Message struct {
	Sender string
	Text   string
	Time   time.Time
}

type messagesByTime []*Message

func (x messagesByTime) Len() int           { return len(x) }
func (x messagesByTime) Less(i, j int) bool { return x[i].Time.Before(x[j].Time) }
func (x messagesByTime) Swap(i, j int)      { x[i], x[j] = x[j], x[i] }

type Patch struct {
	CL          string
	PatchSet    string
	Files       []File `datastore:",noindex"`
	Created     time.Time
	Modified    time.Time
	Owner       string
	NumComments int
	Message     string `datastore:",noindex"`
}

type File struct {
	Status          string
	NumChunks       int
	NoBaseFile      bool
	PropertyChanges string
	NumAdded        int
	NumRemoved      int
	ID              string
	IsBinary        bool
}
