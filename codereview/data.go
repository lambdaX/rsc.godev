// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package codereview

import (
	"regexp"
	"sort"
	"strings"
	"time"

	"app"
)

type CL struct {
	DV int `dataversion:"20"`

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
	Dead bool // CL has been removed
	MessagesLoaded  bool      // Messages are up to date.
	PatchSetsLoaded bool      // PatchSets have been stored (separately).
	HasReviewers    bool      // len(Reviewers) > 0
	Mailed          bool      // 'hg mail' has been run
	Summary         string    // first line of Desc
	Active          bool      // HasReviewers && !Closed && !Submitted
	Repo            string    // repository, learned from patch sets
	Files           []string  // files modified, learned from patch sets
	MoreFiles       bool      // files modified list is truncated (at >100 files)
	FilesModified   time.Time // time of last patch set
	Delta           int64     // lines modified, learned from patch sets
	PrimaryReviewer string    // derived from messages
	NeedsReview     bool      // time for reviewer to look at CL
	LGTM            []string  // lgtms
	NOTLGTM         []string  // not lgtms
	DescIssue []string // issue numbers in latest description
	MailedIssue []string // issues notified about this CL
	NeedMailIssue []string // issues that need mail
}

func isSubmitted(cl *CL) bool {
	for _, m := range cl.Messages {
		if strings.Contains(m.Text, "*** Submitted as") {
			return true
		}
	}
	return false
}

var issueRE = regexp.MustCompile(`(?i)\bissue ([0-9]+)\b`)

func updateCL(cl *CL) {
	cl.parseMessages()
	cl.HasReviewers = len(cl.Reviewers) > 0
	
	cl.Active = cl.Mailed && cl.HasReviewers && !cl.Closed && !cl.Submitted && !cl.Dead &&
		time.Since(cl.Modified) < 365*24*time.Hour &&
		cl.PrimaryReviewer != "close"

	cl.DescIssue = nil
	for _, m := range issueRE.FindAllStringSubmatch(cl.Desc, -1) {
		cl.DescIssue = append(cl.DescIssue, m[1])
	}
	sort.Strings(cl.DescIssue)
	sort.Strings(cl.MailedIssue)

	cl.NeedMailIssue = nil
	if cl.Active {
		mailed := make(map[string]bool)
		for _, issue := range cl.MailedIssue {
			mailed[issue] = true
		}
		for _, issue := range cl.DescIssue {
			if !mailed[issue] {
				cl.NeedMailIssue = append(cl.NeedMailIssue, issue)
				mailed[issue] = true
			}
		}
		sort.Strings(cl.NeedMailIssue)
	}

	if cl.Dead {
		cl.MessagesLoaded = true
		cl.PatchSetsLoaded = true
	}

	if strings.HasPrefix(cl.Repo, "code.google.com/p/go.") || cl.Repo == "code.google.com/p/go" {
		cl.Repo = strings.TrimPrefix(cl.Repo, "code.google.com/p/")
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
	Name            string
	Status          string
	NumChunks       int
	NoBaseFile      bool
	PropertyChanges string
	NumAdded        int
	NumRemoved      int
	ID              string
	IsBinary        bool
}

type dirCount struct {
	name  string
	count int
}

type dirCounts []dirCount

func (x dirCounts) Len() int      { return len(x) }
func (x dirCounts) Swap(i, j int) { x[i], x[j] = x[j], x[i] }
func (x dirCounts) Less(i, j int) bool {
	if x[i].count != x[j].count {
		return x[i].count > x[j].count
	}
	return x[i].name < x[j].name
}

// Dirs returns the list of directories that this CL might be said to be about,
// in preference order.
func (cl *CL) Dirs() []string {
	prefix := ""
	if strings.HasPrefix(cl.Repo, "go.") {
		prefix = cl.Repo + "/"
	} else if cl.Repo != "" && cl.Repo != "go" {
		prefix = cl.Repo + "/"
	}
	counts := map[string]int{}
	for _, file := range cl.Files {
		name := file
		i := strings.LastIndex(name, "/")
		if i >= 0 {
			name = name[:i]
		} else {
			name = ""
		}
		name = strings.TrimPrefix(name, "src/pkg/")
		name = strings.TrimPrefix(name, "src/")
		if name == "src" {
			name = ""
		}
		name = prefix + name
		if name == "" {
			name = "build"
		}
		counts[name]++
	}

	if _, ok := counts["test"]; ok {
		counts["test"] -= 10000 // do not pick as most frequent
	}

	var dirs dirCounts
	for name, count := range counts {
		dirs = append(dirs, dirCount{name, count})
	}
	sort.Sort(dirs)

	var names []string
	for _, d := range dirs {
		names = append(names, d.name)
	}
	return names
}

var (
	reviewerRE = regexp.MustCompile(`(?m)^(?:TB)?R=([\w\-.]+)\b`)
	qRE        = regexp.MustCompile(`(?m)^Q=(\w+)\b`)
	lgtmRE     = regexp.MustCompile(`(?im)^LGTM`)
	notlgtmRE  = regexp.MustCompile(`(?im)^NOT LGTM`)
	helloRE    = regexp.MustCompile(`(?m)Hello ([\w\-.]+)[ ,@][^\n]*\s+^I'd like you to review this change`)
	helloRepoRE = regexp.MustCompile(`(?m)Hello[^\n]+\n\nI'd like you to review this change to\nhttps?://(?:[^/]*@)?(code.google.com/[pr]/[a-z0-9_.\-]+)`)
	helloRepoRE2 = regexp.MustCompile(`(?m)Hello[^\n]+\n\nI'd like you to review this change to\nhttps?://(?:[^/]*@)?([a-z0-9_\-]+)\.googlecode\.com`)
	ptalRE     = regexp.MustCompile(`(?im)^(PTAL|Please take a(nother)? look|I'd like you to review this change)`)
)

func stringKeys(m map[string]bool) []string {
	var x []string
	for k := range m {
		x = append(x, k)
	}
	sort.Strings(x)
	return x
}

var committers = []string{
	// https://code.google.com/p/go/people/list
	// Last updated: 2013-12-17
	"0xe2.0x9a.0x9b@gmail.com",
	"adg@golang.org",
	"adonovan@google.com",
	"agl@golang.org",
	"alex.brainman@gmail.com",
	"ality@pbrane.org",
	"bgarcia@golang.org",
	"bradfitz@golang.org",
	"campoy@golang.org",
	"cmang@golang.org",
	"crawshaw@google.com",
	"cshapiro@golang.org",
	"daniel.morsing@gmail.com",
	"dave@cheney.net",
	"djd@golang.org",
	"dsymonds@golang.org",
	"dvyukov@google.com",
	"gri@golang.org",
	"hectorchu@gmail.com",
	"iant@golang.org",
	"jdpoirier@gmail.com",
	"jsing@google.com",
	"ken@golang.org",
	"khr@golang.org",
	"lvd@golang.org",
	"mikesamuel@gmail.com",
	"mikioh.mikioh@gmail.com",
	"minux.ma@gmail.com",
	"mpvl@golang.org",
	"n13m3y3r@gmail.com",
	"nigeltao@golang.org",
	"pjw@golang.org",
	"r@golang.org",
	"remyoudompheng@gmail.com",
	"rminnich@gmail.com",
	"rogpeppe@gmail.com",
	"rsc@golang.org",
	"sameer@golang.org",
}

func IsReviewer(email string) string {
	return isReviewer(email)
}

func isReviewer(email string) string {
	other := ""
	if strings.HasSuffix(email, "@google.com") {
		other = strings.TrimSuffix(email, "@google.com") + "@golang.org"
	}
	for _, c := range committers {
		if c == email || c == other {
			return c
		}

	}
	return ""
}

func ExpandReviewer(short string) string {
	return expandReviewer(short)
}

func expandReviewer(short string) string {
	if strings.Contains(short, "@") {
		return isReviewer(short)
	}
	for _, c := range committers {
		if i := strings.Index(c, "@"); i >= 0 && c[:i] == short {
			return c
		}
	}
	return ""
}

// parseMessages updates CL state based on parsing the messages.
func (cl *CL) parseMessages() {
	// Determine reviewer and LGTM / not-LGTM.
	// Priority:
	//	1. If submitted, the LGTMers.
	//	2. Last explicit R= in review message.
	//	3. Initial target of review request.
	//	4. Whoever responds first and looks like a reviewer.
	var (
		lgtm             = make(map[string]bool)
		notlgtm          = make(map[string]bool)
		initialReviewer  = ""
		firstResponder   = ""
		explicitReviewer = ""
	)

	cl.Mailed = false
	cl.Submitted = false
	for _, m := range cl.Messages {
		if isReviewer(m.Sender) != "" {
			if notlgtmRE.MatchString(m.Text) {
				notlgtm[m.Sender] = true
				delete(lgtm, m.Sender)
			} else if lgtmRE.MatchString(m.Text) {
				lgtm[m.Sender] = true
				delete(notlgtm, m.Sender)
			}
		}
		if m := helloRE.FindStringSubmatch(m.Text); m != nil {
			cl.Mailed = true
			if x := expandReviewer(m[1]); x != "" {
				initialReviewer = x
			}
		}
		if m := helloRepoRE.FindStringSubmatch(m.Text); m != nil && cl.Repo == "" {
			cl.Repo = m[1]
		}
		if m := helloRepoRE2.FindStringSubmatch(m.Text); m != nil && cl.Repo == "" {
			cl.Repo = "code.google.com/p/" + m[1]
		}
		if strings.Contains(m.Text, "*** Submitted as") {
			cl.Submitted = true
		}
		if m := reviewerRE.FindStringSubmatch(m.Text); m != nil {
			if m[1] == "close" {
				explicitReviewer = "close"
			} else if m[1] == "golang-dev" || m[1] == "golang-codereviews" {
				explicitReviewer = "golang-dev"
			} else if x := expandReviewer(m[1]); x != "" {
				explicitReviewer = x
			}
		}
		if s := isReviewer(m.Sender); s != "" && m.Sender != cl.OwnerEmail && isReviewer(cl.OwnerEmail) != s && firstResponder == "" {
			firstResponder = s
		}
	}

	cl.LGTM = stringKeys(lgtm)
	cl.NOTLGTM = stringKeys(notlgtm)

	cl.PrimaryReviewer = ""
	switch {
	case cl.Submitted && len(cl.LGTM) > 0:
		cl.PrimaryReviewer = cl.LGTM[0]
	case explicitReviewer != "":
		cl.PrimaryReviewer = explicitReviewer
	case initialReviewer != "":
		cl.PrimaryReviewer = initialReviewer
	case firstResponder != "":
		cl.PrimaryReviewer = firstResponder
	}
	
	if cl.PrimaryReviewer == "golang-dev" {
		cl.PrimaryReviewer = ""
	}

	// Now that we know who the primary reviewer is,
	// figure out whether this CL is in need of review
	// (or else is waiting for the author to do more work).
	if cl.Submitted {
		cl.NeedsReview = len(cl.LGTM) == 0
	} else {
		cl.NeedsReview = false
		for _, m := range cl.Messages {
			if ptalRE.MatchString(m.Text) {
				cl.NeedsReview = true
			}
			if m.Sender == cl.PrimaryReviewer {
				cl.NeedsReview = false
			}
		}
	}
}
