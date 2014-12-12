// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Derived from code.google.com/p/rsc/cmd/issue.

package issue

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"app"

	"appengine"
	"appengine/datastore"
	"appengine/urlfetch"

	"github.com/rsc/appstats"
)

func init() {
	http.Handle("/admin/issue/show/", appstats.NewHandler(show))
}

func show(ctxt appengine.Context, w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	var issue Issue
	err := app.ReadData(ctxt, "Issue", strings.TrimPrefix(req.URL.Path, "/admin/issue/show/"), &issue)
	if err != nil {
		fmt.Fprintf(w, "loading issue: %v\n", err)
		return
	}
	js, err := json.Marshal(issue)
	if err != nil {
		fmt.Fprintf(w, "encoding issue to JSON: %v\n", err)
		return
	}
	var buf bytes.Buffer
	json.Indent(&buf, js, "", "\t")
	w.Write(buf.Bytes())
}

func init() {
	app.RegisterStatus("issue loading", status)
}

func status(ctxt appengine.Context) string {
	w := new(bytes.Buffer)

	var count int64
	app.ReadMeta(ctxt, "issue.count", &count)
	fmt.Fprintln(w, time.Now())

	var t1 string
	app.ReadMeta(ctxt, "issue.mtime", &t1)
	fmt.Fprintf(w, "issue modifications up to %v\n", t1)
	fmt.Fprintln(w, time.Now())

	return "<pre>" + html.EscapeString(w.String()) + "</pre>"
}

func init() {
	app.Cron("issue.load", 5*time.Minute, load)

	http.Handle("/admin/issueload", appstats.NewHandler(func(ctxt appengine.Context, w http.ResponseWriter, req *http.Request) { load(ctxt) }))
}

func load(ctxt appengine.Context) error {
	mtime := time.Date(2009, 1, 1, 0, 0, 0, 0, time.UTC)
	if appengine.IsDevAppServer() {
		mtime = time.Now().UTC().Add(-24 * time.Hour)
	}
	app.ReadMeta(ctxt, "issue.mtime", &mtime)

	now := time.Now()

	var issues []*Issue
	var err error
	const maxResults = 500
	var try int
	needMore := false
	for try = 0; ; try++ {
		issues, err = search(ctxt, "go", "all", "", false, mtime, now, maxResults)
		if err != nil {
			ctxt.Errorf("load issues since %v: %v", mtime, err)
			return nil
		}

		if len(issues) == 0 {
			ctxt.Infof("no updates found from %v to %v", mtime, now)
			app.WriteMeta(ctxt, "issue.mtime", now.Add(-1*time.Minute))
			if try > 0 {
				// We shortened the time range; try again now that we've updated mtime.
				return app.ErrMoreCron
			}
			return nil
		}
		if len(issues) < maxResults {
			ctxt.Infof("%d issues from %v to %v", len(issues), mtime, now)
			if try > 0 {
				// Keep exploring once we finish this load.
				needMore = true
			}
			break
		}

		ctxt.Errorf("updater found too many updates from %v to %v", mtime, now)
		if now.Sub(mtime) <= 2*time.Second {
			ctxt.Errorf("cannot shorten update time frame")
			return nil
		}

		if now.Sub(mtime) > 1*time.Hour {
			now = mtime.Add(now.Sub(mtime) / 10)
		} else {
			now = mtime.Add(now.Sub(mtime) / 2)
		}
		ctxt.Infof("shortened to %v to %v", mtime, now)
	}

	issues, err = search(ctxt, "go", "all", "", true, mtime, now, maxResults)
	if err != nil {
		ctxt.Errorf("full load of issues from %v to %v: %v", mtime, now, err)
		return nil
	}

	if len(issues) == 0 {
		ctxt.Errorf("unexpected: no issues from %v to %v", mtime, now)
		return nil
	}

	for _, issue := range issues {
		println("WRITE ISSUE", issue.ID)
		if err := writeIssue(ctxt, issue, "", nil); err != nil {
			return nil
		}
		if mtime.Before(issue.Modified) {
			mtime = issue.Modified
		}
	}

	if try > 0 {
		mtime = now.Add(-1 * time.Second)
	}
	app.WriteMeta(ctxt, "issue.mtime", mtime.UTC())

	if needMore {
		return app.ErrMoreCron
	}
	return nil
}

func writeIssue(ctxt appengine.Context, issue *Issue, stateKey string, state interface{}) error {
	err := app.Transaction(ctxt, func(ctxt appengine.Context) error {
		var old Issue
		if err := app.ReadData(ctxt, "Issue", fmt.Sprint(issue.ID), &old); err != nil && err != datastore.ErrNoSuchEntity {
			return err
		}
		if old.ID == 0 { // no old data
			var count int64
			app.ReadMeta(ctxt, "issue.count", &count)
			app.WriteMeta(ctxt, "issue.count", count+1)
		}

		if old.Modified.After(issue.Modified) {
			return fmt.Errorf("issue %v: have %v but code.google.com sent %v", issue.ID, old.Modified, issue.Modified)
		}

		// Copy Issue into original structure.
		// This allows us to maintain other information in the Issue structure
		// and not overwrite it when the issue information is updated.
		old.ID = issue.ID
		old.Summary = issue.Summary
		old.Status = issue.Status
		old.Duplicate = issue.Duplicate
		old.Owner = issue.Owner
		old.CC = issue.CC
		old.Label = issue.Label
		old.Comment = issue.Comment
		old.State = issue.State
		old.Created = issue.Created
		old.Modified = issue.Modified
		old.Stars = issue.Stars
		old.ClosedDate = issue.ClosedDate
		updateIssue(&old)

		if err := app.WriteData(ctxt, "Issue", fmt.Sprint(issue.ID), &old); err != nil {
			return err
		}
		if stateKey != "" {
			app.WriteMeta(ctxt, stateKey, state)
		}
		return nil
	})
	if err != nil {
		ctxt.Errorf("storing issue %v: %v", issue.ID, err)
	}
	return err
}

func init() {
	app.RegisterDataUpdater("Issue", updateIssue)
}

func updateIssue(issue *Issue) {
	for _, label := range issue.Label {
		if label == "IssueMoved" {
			return
		}
	}
	issue.NeedGithubNote = true
}

type _Feed struct {
	Entry []_Entry `xml:"entry"`
}

type _Entry struct {
	ID         string    `xml:"id"`
	State      string    `xml:"state"`
	Stars      int       `xml:"stars"`
	ClosedDate time.Time `xml:"closedDate"`
	Title      string    `xml:"title"`
	Published  time.Time `xml:"published"`
	Updated    time.Time `xml:"updated"`
	Content    string    `xml:"content"`
	Updates    []_Update `xml:"updates"`
	Author     struct {
		Name string `xml:"name"`
	} `xml:"author"`
	Owner      string   `xml:"owner>username"`
	Status     string   `xml:"status"`
	Label      []string `xml:"label"`
	MergedInto string   `xml:"mergedInto"`
	CC         []string `xml:"cc>username"`

	Dir      string
	Number   int
	Comments []_Entry
}

type _Update struct {
	Summary    string   `xml:"summary"`
	Owner      string   `xml:"ownerUpdate"`
	Label      string   `xml:"label"`
	Status     string   `xml:"status"`
	MergedInto string   `xml:"mergedInto"`
	CC         []string `xml:"cc>username"`
}

var xmlDebug = false

// search queries for issues on the tracker for the given project (for example, "go").
// The can string is typically "open" (search only open issues) or "all" (search all issues).
// The format of the can string and the query are documented at
// https://code.google.com/p/support/wiki/IssueTrackerAPI.
func search(ctxt appengine.Context, project, can, query string, detail bool, updateMin, updateMax time.Time, maxResults int) ([]*Issue, error) {
	client := urlfetch.Client(ctxt)
	if client == nil {
		client = http.DefaultClient
	}
	q := url.Values{
		"q":           {query},
		"max-results": {"1000"},
		"can":         {can},
	}
	if !updateMin.IsZero() {
		q["updated-min"] = []string{updateMin.UTC().Format(time.RFC3339)}
	}
	if !updateMax.IsZero() {
		q["updated-max"] = []string{updateMax.UTC().Format(time.RFC3339)}
	}
	u := "https://code.google.com/feeds/issues/p/" + project + "/issues/full?" + q.Encode()
	ctxt.Infof("URL %s", u)
	r, err := client.Get(u)
	if err != nil {
		return nil, err
	}

	if xmlDebug {
		io.Copy(os.Stdout, r.Body)
		r.Body.Close()
		return nil, nil
	}

	var feed _Feed
	err = xml.NewDecoder(r.Body).Decode(&feed)
	r.Body.Close()
	if err != nil {
		return nil, err
	}

	var issues []*Issue
	for i := range feed.Entry {
		e := &feed.Entry[i]
		id := e.ID
		if i := strings.Index(id, "id="); i >= 0 {
			id = id[:i+len("id=")]
		}
		n, err := strconv.Atoi(id)
		if err != nil {
			return nil, fmt.Errorf("invalid issue ID %q", id)
		}
		dup, _ := strconv.Atoi(e.MergedInto)
		p := &Issue{
			ID:         n,
			Created:    e.Published,
			Modified:   e.Updated,
			Summary:    strings.Replace(e.Title, "\n", " ", -1),
			Status:     e.Status,
			Duplicate:  dup,
			Owner:      e.Owner,
			CC:         e.CC,
			Label:      e.Label,
			State:      e.State,
			Stars:      e.Stars,
			ClosedDate: e.ClosedDate,
			Comment: []Comment{
				{
					Author: e.Author.Name,
					Time:   e.Published,
					Text:   html.UnescapeString(e.Content),
				},
			},
		}
		issues = append(issues, p)
		if detail {
			u := "https://code.google.com/feeds/issues/p/" + project + "/issues/" + id + "/comments/full"
			r, err := client.Get(u)
			if err != nil {
				return nil, err
			}

			var feed _Feed
			err = xml.NewDecoder(r.Body).Decode(&feed)
			r.Body.Close()
			if err != nil {
				return nil, err
			}

			for i := range feed.Entry {
				e := &feed.Entry[i]
				c := Comment{
					Author: strings.TrimPrefix(e.Title, "Comment by "),
					Time:   e.Published,
					Text:   html.UnescapeString(e.Content),
				}
				var cc, label []string
				for _, up := range e.Updates {
					if up.Summary != "" {
						c.Summary = up.Summary
					}
					if up.Owner != "" {
						c.Owner = up.Owner
					}
					if up.Status != "" {
						c.Status = up.Status
					}
					if up.MergedInto != "" {
						c.Duplicate, _ = strconv.Atoi(up.MergedInto)
					}
					if up.Label != "" {
						label = append(label, up.Label)
					}
					cc = append(cc, up.CC...)
				}
				c.CC = strings.Join(cc, ",")
				c.Label = strings.Join(label, ",")
				p.Comment = append(p.Comment, c)
			}
		}
	}

	sort.Sort(BySummary(issues))
	return issues, nil
}
