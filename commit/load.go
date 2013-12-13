// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// TODO: Convert to use app.Cron and app.ScanData.

package commit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"time"

	"code.google.com/p/go.net/html"

	"app"

	"appengine"
	"appengine/datastore"
	"appengine/delay"
	"appengine/urlfetch"
)

// code.google.com sends times in Mountain View time zone.
// If the location cannot be loaded, assume time.Local is enough
// (because we are probably running on an App Engine server
// with time zone set to Mountain View).
var mtv *time.Location

func init() {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		loc = time.Local
	}
	mtv = loc
}

var laterLoad, laterLoadRev *delay.Function

func init() {
	http.HandleFunc("/admin/commit/load", startLoad)
	http.HandleFunc("/admin/commit/kickoff", initialLoad)
	http.HandleFunc("/admin/commit/status", status)
	http.HandleFunc("/admin/commit/show/", show)

	laterLoad = delay.Func("commit.load", load)
	laterLoadRev = delay.Func("commit.loadrev", loadRev)
}

func status(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	q := datastore.NewQuery("RevTodo").
		Limit(100)

	ctxt := appengine.NewContext(req)
	it := q.Run(ctxt)
	for {
		var todo revTodo
		_, err := it.Next(&todo)
		if err != nil {
			break
		}
		fmt.Fprintf(w, "%+v\n", todo)
	}
}

func show(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	ctxt := appengine.NewContext(req)
	var rev Rev
	err := app.ReadData(ctxt, "Rev", strings.TrimPrefix(req.URL.Path, "/admin/commit/show/"), &rev)
	if err != nil {
		fmt.Fprintf(w, "loading rev: %v\n", err)
		return
	}
	js, err := json.Marshal(rev)
	if err != nil {
		fmt.Fprintf(w, "encoding rev to JSON: %v\n", err)
		return
	}
	var buf bytes.Buffer
	json.Indent(&buf, js, "", "\t")
	w.Write(buf.Bytes())
}

func startLoad(w http.ResponseWriter, req *http.Request) {
	ctxt := appengine.NewContext(req)
	laterLoad.Call(ctxt)
}

func initialLoad(w http.ResponseWriter, req *http.Request) {
	ctxt := appengine.NewContext(req)
	for repoBranch, hash := range initialRoots {
		i := strings.Index(repoBranch, "/")
		if i < 0 {
			ctxt.Errorf("invalid initial root %q - missing slash", repoBranch)
			continue
		}
		repo, branch := repoBranch[:i], repoBranch[i+1:]
		addTodo(ctxt, repo, branch, hash)
	}
}

type revTodo struct {
	DV     int `dataversion:"1"`
	Repo   string
	Hash   string
	Branch string
	Start  time.Time
	Last   time.Time
	Time   time.Time
}

func load(ctxt appengine.Context) {
	q := datastore.NewQuery("RevTodo").
		Filter("Time <", time.Now()).
		Limit(100)

	n := 0
	it := q.Run(ctxt)
	for {
		var r revTodo
		_, err := it.Next(&r)
		if err != nil {
			break
		}
		laterLoadRev.Call(ctxt, r.Repo, r.Branch, r.Hash)
		n++
	}
	ctxt.Infof("load found %d todo", n)
}

func loadRev(ctxt appengine.Context, repo, branch, hash string) {
	n := 0
	for hash != "" {
		hash = loadRevOnce(ctxt, repo, branch, hash)
		if n++; n >= 100 {
			laterLoadRev.Call(ctxt, repo, branch, hash)
			break
		}
	}
	ctxt.Infof("processed %d revisions", n)
}

var errWait = errors.New("todo not scheduled yet")

func loadRevOnce(ctxt appengine.Context, repo, branch, hash string) (nextHash string) {
	ctxt.Infof("load todo %s %s %s", repo, branch, hash)

	// Check that this todo is still valid.
	// If so, extend the expiry time so that no one else tries it while we do.
	// This supercedes the usual use of app.Lock and app.Unlock and also
	// provides a way to rate limit the polling.
	todoKey := fmt.Sprintf("commit.todo.%s.%s", repo, hash)

	err := app.Transaction(ctxt, func(ctxt appengine.Context) error {
		var todo revTodo
		if err := app.ReadData(ctxt, "RevTodo", todoKey, &todo); err != nil {
			return err
		}
		if time.Now().Before(todo.Time) {
			ctxt.Infof("poll %s %s not scheduled until %v", repo, hash, todo.Time)
			return errWait
		}
		dtAll := todo.Time.Sub(todo.Start)
		dtOne := todo.Time.Sub(todo.Last)
		var dtMax time.Duration
		if dtAll < 24*time.Hour {
			dtMax = 5 * time.Minute
		} else if dtAll < 7*24*time.Hour {
			dtMax = 1 * time.Hour
		} else {
			dtMax = 24 * time.Hour
		}
		if dtOne *= 2; dtOne > dtMax {
			dtOne = dtMax
		} else if dtOne == 0 {
			dtOne = 1 * time.Minute
		}
		todo.Last = time.Now()
		todo.Time = todo.Last.Add(dtOne)

		if err := app.WriteData(ctxt, "RevTodo", todoKey, &todo); err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		ctxt.Errorf("skipping poll: %v", err)
		return ""
	}

	r, err := fetchRev(ctxt, repo, hash)
	if err != nil {
		ctxt.Errorf("fetching %v %v: %v", repo, hash, err)
		return ""
	}

	err = app.Transaction(ctxt, func(ctxt appengine.Context) error {
		var old Rev
		if err := app.ReadData(ctxt, "Rev", repo+"."+hash, &old); err != nil && err != datastore.ErrNoSuchEntity {
			return err
		}
		if old.Hash == r.Hash && len(old.Next) == len(r.Next) {
			// up to date
			return nil
		}
		if old.Hash == "" { // no old data
			var count int
			if err := app.ReadMeta(ctxt, "commit.count."+repo, &count); err != nil && err != datastore.ErrNoSuchEntity {
				return err
			}
			count++
			old.Seq = count
			if err := app.WriteMeta(ctxt, "commit.count."+repo, count); err != nil {
				return err
			}
			if r.Branch != branch && len(r.Prev) == 1 {
				ctxt.Infof("detected branch; forcing todo of parent")
				err := writeTodo(ctxt, repo, branch, r.Prev[0], true)
				if err != nil {
					ctxt.Errorf("re-adding todo: %v", err)
				}
				nextHash = r.Prev[0]
			}
		}
		old.Repo = r.Repo
		old.Branch = r.Branch
		// old.Seq already correct; r.Seq is not
		old.Hash = r.Hash
		old.ShortHash = old.Hash[:12]
		old.Prev = r.Prev
		old.Next = r.Next
		old.Author = r.Author
		old.AuthorEmail = r.AuthorEmail
		old.Time = r.Time
		old.Log = r.Log
		old.Files = r.Files

		if err := app.WriteData(ctxt, "Rev", repo+"."+hash, &old); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		ctxt.Errorf("updating %v %v: %v", repo, hash, err)
		return ""
	}

	if r.Next == nil {
		ctxt.Errorf("leaving todo for %s %s - no next yet", repo, hash)
		return ""
	}

	success := true
	forward := false
	for _, next := range r.Next {
		err := addTodo(ctxt, repo, r.Branch, next)
		if err == errDone {
			forward = true
			continue
		}
		if err == errBranched {
			ctxt.Infof("%v -> %v is a branch", r.Hash[:12], next[:12])
			continue
		}
		if err != nil {
			ctxt.Errorf("storing todo for %s %s: %v %p %p", repo, next, err, err, errDone)
			success = false
		}
		forward = true // innocent until proven guilty
		if nextHash == "" {
			nextHash = next
		} else {
			laterLoadRev.Call(ctxt, repo, r.Branch, next)
		}
	}

	if forward && success {
		ctxt.Infof("delete todo %s\n", todoKey)
		app.DeleteData(ctxt, "RevTodo", todoKey)
	} else {
		ctxt.Errorf("leaving todo for %s %s due to errors or branching", repo, hash)
	}

	return nextHash
}

var errDone = errors.New("already done")
var errBranched = errors.New("branched")

func addTodo(ctxt appengine.Context, repo, branch, hash string) error {
	ctxt.Infof("add todo %s %s %s\n", repo, branch, hash)
	return app.Transaction(ctxt, func(ctxt appengine.Context) error {
		var rev Rev
		if err := app.ReadData(ctxt, "Rev", repo+"."+hash, &rev); err != datastore.ErrNoSuchEntity {
			if err == nil {
				if rev.Branch != branch {
					return errBranched
				}
				return errDone
			}
			return err
		}

		return writeTodo(ctxt, repo, branch, hash, false)
	})
}

func writeTodo(ctxt appengine.Context, repo, branch, hash string, force bool) error {
	todoNextKey := fmt.Sprintf("commit.todo.%s.%s", repo, hash)
	if !force {
		if err := app.ReadData(ctxt, "RevTodo", todoNextKey, new(revTodo)); err != datastore.ErrNoSuchEntity {
			if err == nil {
				return errDone
			}
			return err
		}
	}
	now := time.Now()
	todoNext := revTodo{
		Repo:   repo,
		Hash:   hash,
		Branch: branch,
		Start:  now,
		Last:   now,
		Time:   now,
	}
	if err := app.WriteData(ctxt, "RevTodo", todoNextKey, &todoNext); err != nil {
		return err
	}
	return nil
}

func fetchRev(ctxt appengine.Context, repo, hash string) (*Rev, error) {
	http := urlfetch.Client(ctxt)

	url := "https://code.google.com/p/go/source/detail?r=" + hash
	if repo != "main" {
		url += "&repo=" + strings.TrimPrefix(repo, "go.")
	}

	res, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, errors.New(res.Status)
	}
	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	var r Rev
	r.Repo = repo
	r.Hash = hash
	r.ShortHash = r.Hash[:12]
	process(ctxt, &r, doc)

	if r.Author == "" {
		return nil, fmt.Errorf("unable to understand revision html - no author")
	}
	if r.Log == "" {
		return nil, fmt.Errorf("unable to understand revision html - no log")
	}
	if r.Time.IsZero() {
		return nil, fmt.Errorf("unable to understand revision html - no time")
	}

	return &r, nil
}

var (
	hrefDetailRE = regexp.MustCompile(`^detail\?(?:.*&)?r=([0-9a-f]+)`)
	hrefBrowseRE = regexp.MustCompile(`^browse(/[^?]+)\?`)
	hrefAuthorRE = regexp.MustCompile(`^([^<>]+)(?: <(.*)>)?$`)
)

func attr(n *html.Node, name string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

func isElem(n *html.Node, name string) bool {
	return n.Type == html.ElementNode && n.Data == name
}

func findChildElem(n *html.Node, name string) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if isElem(c, name) {
			return c
		}
	}
	return nil
}

func innerText(n *html.Node) string {
	return strings.TrimSpace(string(appendInnerText(nil, n)))
}

func appendInnerText(buf []byte, n *html.Node) []byte {
	if n == nil {
		return buf
	}
	if n.Type == html.TextNode {
		buf = append(buf, n.Data...)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		buf = appendInnerText(buf, c)
	}
	return buf
}

func process(ctxt appengine.Context, rev *Rev, n *html.Node) {
	if isElem(n, "a") && attr(n, "title") == "Next" {
		m := hrefDetailRE.FindStringSubmatch(attr(n, "href"))
		if m == nil {
			ctxt.Infof("cannot find rev in href %q", attr(n, "href"))
			return
		}
		rev.Next = append(rev.Next, m[1])
		return
	}
	if isElem(n, "a") && attr(n, "title") == "Previous" {
		m := hrefDetailRE.FindStringSubmatch(attr(n, "href"))
		if m == nil {
			ctxt.Infof("cannot find rev in href %q", attr(n, "href"))
			return
		}
		rev.Prev = append(rev.Prev, m[1])
		return
	}

	if isElem(n, "tr") {
		th := findChildElem(n, "th")
		td := findChildElem(n, "td")
		if th != nil && td != nil {
			thstr := strings.TrimSpace(innerText(th))
			if strings.HasPrefix(thstr, "Author:") {
				m := hrefAuthorRE.FindStringSubmatch(innerText(td))
				if m != nil {
					rev.Author = m[1]
					rev.AuthorEmail = m[2]
				}
			}
			if strings.HasPrefix(thstr, "Date:") {
				date := attr(findChildElem(td, "span"), "title")
				t, err := time.ParseInLocation(time.ANSIC, date, mtv)
				if err != nil {
					ctxt.Infof("parsing time %q: %v", date, err)
				} else {
					rev.Time = t.UTC()
				}
			}
			if strings.HasPrefix(thstr, "Branch:") {
				rev.Branch = innerText(td)
			}
		}
	}

	if isElem(n, "h4") && innerText(n) == "Log message" {
		for c := n.NextSibling; c != nil; c = c.NextSibling {
			if isElem(c, "h4") {
				break
			}
			if isElem(c, "pre") {
				rev.Log = innerText(c)
				break
			}
		}
	}

	if isElem(n, "tbody") && attr(n, "id") == "files" {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if isElem(c, "tr") {
				for cc := c.FirstChild; cc != nil; cc = cc.NextSibling {
					if isElem(cc, "td") && cc.NextSibling != nil && findChildElem(cc.NextSibling, "a") != nil {
						next := cc.NextSibling
						m := hrefBrowseRE.FindStringSubmatch(attr(findChildElem(next, "a"), "href"))
						if m != nil {
							op := innerText(cc)
							rev.Files = append(rev.Files, File{op, m[1]})
						}
					}
				}
			}
		}
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		process(ctxt, rev, c)
	}
}
