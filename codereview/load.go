// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Derived from github.com/bradfitz/qopher/cmd/gotasks/gotasks.go

package codereview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"app"

	"appengine"
	"appengine/datastore"
	"appengine/urlfetch"

	"github.com/rsc/appstats"
)

type jsonCL struct {
	Issue      int64          `json:"issue"`
	Desc       string         `json:"description"`
	OwnerEmail string         `json:"owner_email"`
	Owner      string         `json:"owner"`
	Created    string         `json:"created"`
	Modified   string         `json:"modified"` // just a string; more reliable to do string equality tests on it
	Messages   []*jsonMessage `json:"messages"`
	Reviewers  []string       `json:"reviewers"`
	CC         []string       `json:"cc"`
	Closed     bool           `json:"closed"`
	PatchSets  []int64        `json:"patchsets"`
}

func parseTime(ctxt appengine.Context, s string) time.Time {
	t, err := time.ParseInLocation(timeFormat, s, time.UTC)
	if err != nil {
		return time.Unix(0, 0)
	}
	return t
}

func (j *jsonCL) toCL(ctxt appengine.Context) *CL {
	cl := &CL{
		CL:         fmt.Sprint(j.Issue),
		Desc:       j.Desc,
		OwnerEmail: j.OwnerEmail,
		Owner:      j.Owner,
		Created:    parseTime(ctxt, j.Created),
		Modified:   parseTime(ctxt, j.Modified),
		Reviewers:  j.Reviewers,
		CC:         j.CC,
		Closed:     j.Closed,
	}
	for _, m := range j.Messages {
		cl.Messages = append(cl.Messages, m.toMessage(ctxt))
	}
	for _, p := range j.PatchSets {
		cl.PatchSets = append(cl.PatchSets, fmt.Sprint(p))
	}
	return cl
}

type jsonMessage struct {
	Sender string `json:"sender"`
	Text   string `json:"text"`
	Date   string `json:"date"` // "2012-04-07 00:51:58.602055"
}

func (j *jsonMessage) toMessage(ctxt appengine.Context) Message {
	return Message{
		Sender: j.Sender,
		Text:   j.Text,
		Time:   parseTime(ctxt, j.Date),
	}
}

type jsonPatch struct {
	Files       map[string]*jsonFile `json:"files"`
	Created     string               `json:"created"`
	Owner       string               `json:"owner"`
	NumComments int                  `json:"num_comments"`
	PatchSet    int64                `json:"patchset"`
	Issue       int64                `json:"issue"`
	Message     string               `json:"message"`
	Modified    string               `json:"modified"`
}

func (j *jsonPatch) toPatch(ctxt appengine.Context) *Patch {
	p := &Patch{
		CL:          fmt.Sprint(j.Issue),
		PatchSet:    fmt.Sprint(j.PatchSet),
		Created:     parseTime(ctxt, j.Created),
		Modified:    parseTime(ctxt, j.Modified),
		Owner:       j.Owner,
		NumComments: j.NumComments,
		Message:     j.Message,
	}
	var names []string
	for name := range j.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p.Files = append(p.Files, j.Files[name].toFile(ctxt, name))
	}
	return p
}

type jsonFile struct {
	Status          string `json:"status"`
	NumChunks       int    `json:"num_chunks"`
	NoBaseFile      bool   `json:"no_base_file"`
	PropertyChanges string `json:"property_changes"`
	NumAdded        int    `json:"num_added"`
	NumRemoved      int    `json:"num_removed"`
	ID              int64  `json:"id"`
	IsBinary        bool   `json:"is_binary"`
}

func (j *jsonFile) toFile(ctxt appengine.Context, name string) File {
	return File{
		Name:            name,
		Status:          j.Status,
		NumChunks:       j.NumChunks,
		NoBaseFile:      j.NoBaseFile,
		PropertyChanges: j.PropertyChanges,
		NumAdded:        j.NumAdded,
		NumRemoved:      j.NumRemoved,
		ID:              fmt.Sprint(j.ID),
		IsBinary:        j.IsBinary,
	}
}

func init() {
	http.Handle("/admin/codereview/show/", appstats.NewHandler(show))
}

func show(ctxt appengine.Context, w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	var cl CL
	err := app.ReadData(ctxt, "CL", strings.TrimPrefix(req.URL.Path, "/admin/codereview/show/"), &cl)
	if err != nil {
		fmt.Fprintf(w, "loading CL: %v\n", err)
		return
	}
	js, err := json.Marshal(cl)
	if err != nil {
		fmt.Fprintf(w, "encoding CL to JSON: %v\n", err)
		return
	}
	var buf bytes.Buffer
	json.Indent(&buf, js, "", "\t")
	w.Write(buf.Bytes())
}

func init() {
	app.Cron("codereview.load", 1*time.Minute, load)
}

func load(ctxt appengine.Context) error {
	// The deadline for task invocation is 10 minutes.
	// Stop when we've run for 5 minutes and ask to be rescheduled.
	deadline := time.Now().Add(5 * time.Minute)

	for _, group := range []string{"golang-dev", "golang-codereviews"} {
		for _, reviewerOrCC := range []string{"reviewer", "cc"} {
			// The stored mtime is the most recent modification time we've seen.
			// We ask for all changes since then.
			mtimeKey := "codereview.mtime." + reviewerOrCC + "." + group
			var mtime string
			if appengine.IsDevAppServer() {
				mtime = "2013-12-01 00:00:00" // limit fetching in empty datastore
			}
			app.ReadMeta(ctxt, mtimeKey, &mtime)
			cursor := ""

			// Rietveld gives us back times with microseconds, but it rejects microseconds
			// in the ModifiedAfter URL parameter. Drop them. We'll see a few of the most
			// recent CLs again. No big deal.
			if i := strings.Index(mtime, "."); i >= 0 {
				mtime = mtime[:i]
			}

			const itemsPerPage = 100
			for n := 0; ; n++ {
				var q struct {
					Cursor  string    `json:"cursor"`
					Results []*jsonCL `json:"results"`
				}
				err := fetchJSON(ctxt, &q, urlWithParams(queryTmpl, map[string]string{
					"ReviewerOrCC":  reviewerOrCC,
					"Group":         group,
					"ModifiedAfter": mtime,
					"Order":         "modified",
					"Cursor":        cursor,
					"Limit":         fmt.Sprint(itemsPerPage),
				}))
				if err != nil {
					ctxt.Errorf("loading codereview by %s: URL <%s>: %v", reviewerOrCC, q, err)
					break
				}
				ctxt.Infof("found %d CLs", len(q.Results))
				if len(q.Results) == 0 {
					break
				}
				cursor = q.Cursor

				for _, jcl := range q.Results {
					cl := jcl.toCL(ctxt)
					if err := writeCL(ctxt, cl, mtimeKey, jcl.Modified); err != nil {
						break // error already logged
					}
				}

				if len(q.Results) < itemsPerPage {
					ctxt.Infof("reached end of results - codereview by %s up to date", reviewerOrCC)
					break
				}

				if time.Now().After(deadline) {
					ctxt.Infof("more to do for codereview by %s - rescheduling", reviewerOrCC)
					return app.ErrMoreCron
				}
			}
		}
	}

	ctxt.Infof("all done")
	return nil
}

func writeCL(ctxt appengine.Context, cl *CL, mtimeKey, modified string) error {
	err := app.Transaction(ctxt, func(ctxt appengine.Context) error {
		var old CL
		if err := app.ReadData(ctxt, "CL", cl.CL, &old); err != nil && err != datastore.ErrNoSuchEntity {
			return err
		}
		if old.CL == "" { // no old data
			var count int64
			app.ReadMeta(ctxt, "codereview.count", &count)
			app.WriteMeta(ctxt, "codereview.count", count+1)
		}

		// Copy CL into original structure.
		// This allows us to maintain other information in the CL structure
		// and not overwrite it when the Rietveld information is updated.
		if cl.Dead {
			old.Dead = true
		} else {
			old.Dead = false
			if old.Modified.After(cl.Modified) {
				return fmt.Errorf("CL %v: have %v but Rietveld sent %v", cl.CL, old.Modified, cl.Modified)
			}
			old.CL = cl.CL
			old.Desc = cl.Desc
			old.Owner = cl.Owner
			old.OwnerEmail = cl.OwnerEmail
			old.Created = cl.Created
			old.Modified = cl.Modified
			old.MessagesLoaded = cl.MessagesLoaded
			if cl.MessagesLoaded {
				old.Messages = cl.Messages
				old.Submitted = cl.Submitted
			}
			old.Reviewers = cl.Reviewers
			old.CC = cl.CC
			old.Closed = cl.Closed
			if !reflect.DeepEqual(old.PatchSets, cl.PatchSets) {
				old.PatchSets = cl.PatchSets
				old.PatchSetsLoaded = false
			}
		}

		if err := app.WriteData(ctxt, "CL", cl.CL, &old); err != nil {
			return err
		}
		if mtimeKey != "" {
			app.WriteMeta(ctxt, mtimeKey, modified)
		}
		return nil
	})
	if err != nil {
		ctxt.Errorf("storing CL %v: %v", cl.CL, err)
	}
	return err
}

func init() {
	app.ScanData("codereview.loadmsg", 1*time.Minute,
		datastore.NewQuery("CL").Filter("MessagesLoaded =", false),
		loadmsg)

	app.ScanData("codereview.loadpatch", 1*time.Minute,
		datastore.NewQuery("CL").Filter("PatchSetsLoaded =", false),
		loadpatch)

	app.ScanData("codereview.mail", 15*time.Minute,
		datastore.NewQuery("CL").Filter("Active =", true).Filter("NeedMailIssue >", ""),
		mailissue)
}

func loadmsg(ctxt appengine.Context, kind, key string) error {
	var jcl jsonCL
	err := fetchJSON(ctxt, &jcl, urlWithParams(issueTmpl, map[string]string{
		"CL": key,
	}))
	if err != nil {
		// Should do a better job returning a distinct error, but this will do for now.
		if strings.Contains(err.Error(), "404 Not Found") {
			cl := &CL{
				CL:   key,
				Dead: true,
			}
			writeCL(ctxt, cl, "", "")
		}
		return nil // error already logged
	}
	cl := jcl.toCL(ctxt)
	cl.MessagesLoaded = true
	writeCL(ctxt, cl, "", "")
	return nil
}

var (
	diffRE  = regexp.MustCompile(`diff -r [0-9a-f]+ https?://(?:[^/]*@)?(code.google.com/[pr]/[a-z0-9_.\-]+)`)
	diffRE2 = regexp.MustCompile(`diff -r [0-9a-f]+ https?://(?:[^/]*@)?([a-z0-9_\-]+)\.googlecode\.com`)
	diffRE3 = regexp.MustCompile(`diff -r [0-9a-f]+ https?://(?:[^/]*@)?([a-z0-9_\-]+)\.([a-z0-9_\-]+)\.googlecode\.com`)
)

func loadpatch(ctxt appengine.Context, kind, key string) error {
	ctxt.Infof("loadpatch %s", key)
	var cl CL
	err := app.ReadData(ctxt, "CL", key, &cl)
	if err != nil {
		return nil // error already logged
	}

	if cl.PatchSetsLoaded {
		return nil
	}

	var last *Patch
	for _, id := range cl.PatchSets {
		var jp jsonPatch
		err := fetchJSON(ctxt, &jp, fmt.Sprintf("https://codereview.appspot.com/api/%s/%s", cl.CL, id))
		if err != nil {
			return nil // already logged
		}
		p := jp.toPatch(ctxt)
		if err := app.WriteData(ctxt, "Patch", fmt.Sprintf("%s/%s", cl.CL, id), p); err != nil {
			return nil // already logged
		}
		last = p
	}

	err = app.Transaction(ctxt, func(ctxt appengine.Context) error {
		var old CL
		if err := app.ReadData(ctxt, "CL", key, &old); err != nil {
			return err
		}
		if len(old.PatchSets) > len(cl.PatchSets) {
			return fmt.Errorf("more patch sets added")
		}
		old.PatchSetsLoaded = true
		old.FilesModified = last.Modified
		old.Files = nil
		old.Delta = 0
		for _, f := range last.Files {
			old.Files = append(old.Files, f.Name)
			old.Delta += int64(f.NumAdded + f.NumRemoved)
		}
		if len(old.Files) > 100 {
			old.Files = old.Files[:100]
			old.MoreFiles = true
		}
		if m := diffRE.FindStringSubmatch(last.Message); m != nil {
			old.Repo = m[1]
		} else if m := diffRE2.FindStringSubmatch(last.Message); m != nil {
			old.Repo = "code.google.com/p/" + m[1]
		} else if m := diffRE3.FindStringSubmatch(last.Message); m != nil {
			old.Repo = "code.google.com/p/" + m[2] + "." + m[1]
		}
		// NOTE: updateCL will shorten code.google.com/p/go to go.
		return app.WriteData(ctxt, "CL", key, &old)
	})
	return err
}

func init() {
	http.Handle("/admin/codereview/mailissue", appstats.NewHandler(testmailissue))
}

func testmailissue(ctxt appengine.Context, w http.ResponseWriter, req *http.Request) {
	err := mailissue(ctxt, "CL", req.FormValue("cl"))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	fmt.Fprintf(w, "OK!\n")
}

func mailissue(ctxt appengine.Context, kind, key string) error {
	ctxt.Infof("mailissue %s", key)
	var cl CL
	err := app.ReadData(ctxt, "CL", key, &cl)
	if err != nil {
		return nil // error already logged
	}

	if len(cl.NeedMailIssue) == 0 {
		return nil
	}

	var mailed []string
	for _, issue := range cl.NeedMailIssue {
		err := postIssueComment(ctxt, issue, "CL https://codereview.appspot.com/"+cl.CL+" mentions this issue.")
		if err != nil {
			ctxt.Criticalf("posting to issue %v: %v", issue, err)
			continue
		}
		mailed = append(mailed, issue)
	}

	err = app.Transaction(ctxt, func(ctxt appengine.Context) error {
		var old CL
		if err := app.ReadData(ctxt, "CL", key, &old); err != nil {
			return err
		}
		old.MailedIssue = append(old.MailedIssue, mailed...)
		return app.WriteData(ctxt, "CL", key, &old)
	})

	return err
}

func fetchJSON(ctxt appengine.Context, target interface{}, url string) error {
	http := urlfetch.Client(ctxt)

	res, err := http.Get(url)
	if err != nil {
		ctxt.Errorf("fetch URL <%s>: %v", url, err)
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		ctxt.Errorf("fetch URL <%s>: %v", url, res.Status)
		return fmt.Errorf("http %v", res.Status)
	}

	err = json.NewDecoder(res.Body).Decode(target)
	if err != nil {
		ctxt.Errorf("decoding JSON from URL <%s>: %v", url, err)
		return err
	}
	return nil
}

const (
	maxItemsPerPage = 1000

	timeFormat = "2006-01-02 15:04:05"

	// closed=1 means "unknown"
	queryTmpl = "https://codereview.appspot.com/search?closed=1&owner=&{{ReviewerOrCC}}={{Group}}@googlegroups.com&repo_guid=&base=&private=1&created_before=&created_after=&modified_before=&modified_after={{ModifiedAfter}}&order={{Order}}&format=json&keys_only=False&with_messages=False&cursor={{Cursor}}&limit={{Limit}}"

	// JSON with the text of messages. e.g.
	// https://codereview.appspot.com/api/6454085?messages=true
	issueTmpl = "https://codereview.appspot.com/api/{{CL}}?messages=true"
)

// itemsPerPage is the number of items to fetch for a single page.
// Changed by tests.
var itemsPerPage = 100 // maxItemsPerPage

var urlParam = regexp.MustCompile(`{{\w+}}`)

func urlWithParams(urlTempl string, m map[string]string) string {
	return urlParam.ReplaceAllStringFunc(urlTempl, func(param string) string {
		return url.QueryEscape(m[strings.Trim(param, "{}")])
	})
}

func init() {
	app.RegisterStatus("codereview", status)
}

func status(ctxt appengine.Context) string {
	w := new(bytes.Buffer)
	var count int64
	for _, group := range []string{"golang-dev", "golang-codereviews"} {
		for _, reviewerOrCC := range []string{"reviewer", "cc"} {
			var t string
			mtimeKey := "codereview.mtime." + reviewerOrCC + "." + group
			app.ReadMeta(ctxt, mtimeKey, &t)
			fmt.Fprintf(w, "%v last update for %s\n", t, mtimeKey)
		}
	}
	app.ReadMeta(ctxt, "codereview.count", &count)
	fmt.Fprintf(w, "%d CLs total\n", count)

	var chunk = 20000
	if appengine.IsDevAppServer() {
		chunk = 100
	}
	q := datastore.NewQuery("CL").
		Filter("PatchSetsLoaded <=", false).
		KeysOnly().
		Limit(chunk)

	n := 0
	it := q.Run(ctxt)
	for {
		_, err := it.Next(nil)
		if err != nil {
			break
		}
		n++
	}
	fmt.Fprintf(w, "%d with PatchSetsLoaded = false\n", n)

	q = datastore.NewQuery("CL").
		Filter("MessagesLoaded <=", false).
		KeysOnly().
		Limit(chunk)

	n = 0
	it = q.Run(ctxt)
	for {
		_, err := it.Next(nil)
		if err != nil {
			break
		}
		n++
	}
	fmt.Fprintf(w, "%d with MessagesLoaded = false\n", n)

	fmt.Fprintf(w, "\n")

	q = datastore.NewQuery("RevTodo").
		Limit(10000).
		KeysOnly()

	n = 0
	it = q.Run(ctxt)
	for {
		_, err := it.Next(nil)
		if err != nil {
			break
		}
		n++
	}
	fmt.Fprintf(w, "\n%d hg heads\n", n)

	q = datastore.NewQuery("Meta").
		Filter("__key__ >=", datastore.NewKey(ctxt, "Meta", "commit.count.", 0, nil)).
		Filter("__key__ <=", datastore.NewKey(ctxt, "Meta", "commit.count/", 0, nil)).
		Limit(100)

	type meta struct {
		JSON []byte `datastore:",noindex"`
	}
	it = q.Run(ctxt)
	for {
		var m meta
		key, err := it.Next(&m)
		if err != nil {
			break
		}
		fmt.Fprintf(w, "%s %s\n", key.StringID(), m.JSON)
	}
	fmt.Fprintf(w, "\n")

	q = datastore.NewQuery("CL").
		Filter("Closed =", false).
		Filter("Submitted =", false).
		Filter("HasReviewers =", true).
		Order("Summary").
		KeysOnly().
		Limit(20000)

	n = 0
	it = q.Run(ctxt)
	for {
		_, err := it.Next(nil)
		if err != nil {
			break
		}
		n++
	}
	fmt.Fprintf(w, "\n%d pending CLs.\n", n)

	q = datastore.NewQuery("CL").
		Filter("Active =", true).
		Filter("NeedMailIssue >", "").
		KeysOnly().
		Limit(20000)

	n = 0
	it = q.Run(ctxt)
	for {
		_, err := it.Next(nil)
		if err != nil {
			break
		}
		n++
	}
	fmt.Fprintf(w, "\n%d CLs need issue mails.\n", n)

	return "<pre>" + html.EscapeString(w.String()) + "</pre>\n"
}
