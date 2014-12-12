// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dash

import (
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"app"
	"codereview"
	"issue"

	"appengine"
	"appengine/datastore"
	"appengine/user"

	"github.com/rsc/appstats"
)

func init() {
	http.Handle("/", appstats.NewHandler(showDash))
	http.Handle("/uiop", appstats.NewHandler(uiOperation))
}

type Group struct {
	Dir   string
	Items []*Item
}

type Item struct {
	Bug *issue.Issue
	CLs []*codereview.CL
}

type itemsBySummary []*Item

func (x itemsBySummary) Len() int           { return len(x) }
func (x itemsBySummary) Swap(i, j int)      { x[i], x[j] = x[j], x[i] }
func (x itemsBySummary) Less(i, j int) bool { return itemSummary(x[i]) < itemSummary(x[j]) }

func itemSummary(it *Item) string {
	if it.Bug != nil {
		return it.Bug.Summary
	}
	for _, cl := range it.CLs {
		return cl.Summary
	}
	return ""
}

func dirKey(s string) string {
	if strings.Contains(s, ".") {
		return "\x7F" + s
	}
	return s
}

// display holds state needed to compute the displayed HTML.
// The methods here are turned into functions for the template to call.
// Not all methods need the display state; being methods just keeps
// them all in one place.
type display struct {
	email string
	pref  UserPref
}

// UserPref holds user preferences; stored in the datastore under email address.
type UserPref struct {
	Muted []string
}

// short returns a shortened email address by removing @domain.
// Input can be string or []string; output is same.
func (d *display) short(s interface{}) interface{} {
	switch s := s.(type) {
	case string:
		if i := strings.Index(s, "@"); i >= 0 {
			return s[:i]
		}
		return s
	case []string:
		v := make([]string, len(s))
		for i, t := range s {
			v[i] = d.short(t).(string)
		}
		return v
	default:
		return s
	}
	return s
}

// css returns name if cond is true; otherwise it returns the empty string.
// It is intended for use in generating css class names (or not).
func (d *display) css(name string, cond bool) string {
	if cond {
		return name
	}
	return ""
}

// old returns css class "old" t is too long ago.
func (d *display) old(t time.Time) string {
	return d.css("old", time.Since(t) > 7*24*time.Hour)
}

// join is like strings.Join but takes arguments in the reverse order,
// enabling {{list | join ","}}.
func (d *display) join(sep string, list []string) string {
	return strings.Join(list, sep)
}

// since returns the elapsed time since t as a number of days.
func (d *display) since(t time.Time) string {
	// NOTE: Considered changing the unit (hours, days, weeks)
	// but that made it harder to scan through the table.
	// If it's always days, that's one less thing you have to read.
	// Otherwise 1 week might be misread as worse than 6 hours.
	dt := time.Since(t)
	return fmt.Sprintf("%.1f days ago", float64(dt)/float64(24*time.Hour))
}

// reviewer returns the reviewer for a CL:
// the actual reviewer if there is one, or else "golang-dev".
func (d *display) reviewer(cl *codereview.CL) string {
	if cl.PrimaryReviewer == "" {
		return "golang-dev"
	}
	return cl.PrimaryReviewer
}

// second returns the css class "second" if the index is non-zero
// (so really "second" here means "not first").
func (d *display) second(index int) string {
	return d.css("second", index > 0)
}

// mine returns the css class "mine" if the email address is the logged-in user.
// It also returns "unassigned" for the unassigned reviewer "golang-dev"
// (see reviewer above).
func (d *display) mine(email string) string {
	if email == d.email {
		return "mine"
	}
	if email == "golang-dev" {
		return "unassigned"
	}
	return ""
}

// muted returns the css class "muted" if the directory is muted.
func (d *display) muted(dir string) string {
	for _, m := range d.pref.Muted {
		if m == dir {
			return "muted"
		}
	}
	return ""
}

func findEmail(ctxt appengine.Context) string {
	self := ""
	u := user.Current(ctxt)
	if u != nil {
		self = codereview.IsReviewer(u.Email)
		if self == "" {
			self = u.Email
		}
	}
	return self
}

func showDash(ctxt appengine.Context, w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/login" {
		http.Redirect(w, req, "/", 302)
		return
	}
	if req.URL.Path != "/" {
		http.ServeFile(w, req, "static/"+req.URL.Path)
		return
	}
	const chunk = 1000
	ctxt.Errorf("DASH")
	req.ParseForm()

	var cls []*codereview.CL
	_, err := datastore.NewQuery("CL").
		Filter("Active =", true).
		Limit(chunk).
		GetAll(ctxt, &cls)
	if err != nil {
		ctxt.Errorf("loading CLs: %v", err)
		fmt.Fprintf(w, "loading CLs failed\n")
		return
	}

	var bugs []*issue.Issue
	_, err = datastore.NewQuery("Issue").
		Filter("State =", "open").
		Filter("Label =", "Release-Go1.3").
		Limit(chunk).
		GetAll(ctxt, &bugs)
	if err != nil {
		ctxt.Errorf("loading issues: %v", err)
		fmt.Fprintf(w, "loading issues failed\n")
		return
	}

	groups := make(map[string]*Group)
	itemsByBug := make(map[int]*Item)

	addGroup := func(item *Item) {
		dir := itemDir(item)
		g := groups[dirKey(dir)]
		if g == nil {
			g = &Group{Dir: dir}
			groups[dirKey(dir)] = g
		}
		g.Items = append(g.Items, item)
	}

	for _, bug := range bugs {
		item := &Item{Bug: bug}
		addGroup(item)
		itemsByBug[bug.ID] = item
	}

	for _, cl := range cls {
		found := false
		for _, id := range clBugs(cl) {
			item := itemsByBug[id]
			if item != nil {
				found = true
				item.CLs = append(item.CLs, cl)
			}
		}
		if !found {
			item := &Item{CLs: []*codereview.CL{cl}}
			addGroup(item)
		}
	}

	for _, g := range groups {
		sort.Sort(itemsBySummary(g.Items))
	}

	// Load information about logged-in user.
	var d display
	d.email = findEmail(ctxt)
	if d.email != "" {
		app.ReadData(ctxt, "UserPref", d.email, &d.pref)
	}

	/*

		nrow := 0
		self := findEmail(ctxt)
		isme := func(s string) bool { return s == self || s == "golang-dev" }

		muted := func(dir string) string {
			for _, targ := range pref.Muted {
				if dir == targ {
					return "muted"
				}
			}
			return ""
		}
		todo := func(cl *codereview.CL) bool {
			if cl.NeedsReview {
				return isme(defaultReviewer(cl))
			} else {
				return isme(cl.OwnerEmail)
			}
		}
		todoItem := func(item *Item) bool {
			for _, cl := range item.CLs {
				if todo(cl) {
					return true
				}
			}
			return false
		}
		todoGroup := func(g *Group) bool {
			for _, item := range g.Items {
				if todoItem(item) {
					return true
				}
			}
			return false
		}
	*/

	tmpl, err := ioutil.ReadFile("template/dash.html")
	if err != nil {
		ctxt.Errorf("reading template: %v", err)
		return
	}
	t, err := template.New("main").Funcs(template.FuncMap{
		"css":      d.css,
		"join":     d.join,
		"mine":     d.mine,
		"muted":    d.muted,
		"old":      d.old,
		"replace":  strings.Replace,
		"reviewer": d.reviewer,
		"second":   d.second,
		"short":    d.short,
		"since":    d.since,
	}).Parse(string(tmpl))
	if err != nil {
		ctxt.Errorf("parsing template: %v", err)
		return
	}

	data := struct {
		User string
		Dirs map[string]*Group
	}{
		d.email,
		groups,
	}

	if err := t.Execute(w, data); err != nil {
		ctxt.Errorf("execute: %v", err)
		fmt.Fprintf(w, "error executing template\n")
		return
	}
}

func descDir(desc string) string {
	desc = strings.TrimSpace(desc)
	i := strings.Index(desc, ":")
	if i < 0 {
		return ""
	}
	desc = desc[:i]
	if i := strings.Index(desc, ","); i >= 0 {
		desc = strings.TrimSpace(desc[:i])
	}
	if strings.Contains(desc, " ") {
		return ""
	}
	return desc
}

var okDesc = map[string]bool{
	"all":   true,
	"build": true,
}

func itemDir(item *Item) string {
	for _, cl := range item.CLs {
		dirs := cl.Dirs()
		desc := descDir(cl.Summary)

		// Accept description if it is a global prefix like "all".
		if okDesc[desc] {
			return desc
		}

		// Accept description if it matches one of the directories.
		for _, dir := range dirs {
			if dir == desc {
				return dir
			}
		}

		// Otherwise use most common directory.
		if len(dirs) > 0 {
			return dirs[0]
		}

		// Otherwise accept description.
		return desc
	}
	if item.Bug != nil {
		if dir := descDir(item.Bug.Summary); dir != "" {
			return dir
		}
		return "?"
	}
	return "?"
}

var bugRE = regexp.MustCompile(`Fixes issue (\d+)`)

func clBugs(cl *codereview.CL) []int {
	var out []int
	for _, m := range bugRE.FindAllStringSubmatch(cl.Desc, -1) {
		n, _ := strconv.Atoi(m[1])
		if n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func uiOperation(ctxt appengine.Context, w http.ResponseWriter, req *http.Request) {
	email := findEmail(ctxt)
	d := display{email: email}
	if d.email == "" {
		w.WriteHeader(501)
		fmt.Fprintf(w, "must be logged in")
		return
	}
	if req.Method != "POST" {
		w.WriteHeader(501)
		fmt.Fprintf(w, "must POST")
		return
	}
	// TODO: XSRF protection
	switch op := req.FormValue("op"); op {
	default:
		w.WriteHeader(501)
		fmt.Fprintf(w, "invalid verb")
		return
	case "mute", "unmute":
		targ := req.FormValue("dir")
		if targ == "" {
			w.WriteHeader(501)
			fmt.Fprintf(w, "missing dir")
			return
		}
		err := app.Transaction(ctxt, func(ctxt appengine.Context) error {
			var pref UserPref
			app.ReadData(ctxt, "UserPref", d.email, &pref)
			for i, dir := range pref.Muted {
				if dir == targ {
					if op == "unmute" {
						pref.Muted = append(pref.Muted[:i], pref.Muted[i+1:]...)
						break
					}
					return nil
				}
			}
			if op == "mute" {
				pref.Muted = append(pref.Muted, targ)
				sort.Strings(pref.Muted)
			}
			return app.WriteData(ctxt, "UserPref", d.email, &pref)
		})
		if err != nil {
			w.WriteHeader(501)
			fmt.Fprintf(w, "unable to update")
			return
		}

	case "reviewer":
		clnum := req.FormValue("cl")
		who := req.FormValue("reviewer")
		switch who {
		case "close", "golang-dev":
			// ok
		default:
			who = codereview.ExpandReviewer(who)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if who == "" {
			fmt.Fprintf(w, "ERROR: unknown reviewer")
			return
		}
		if err := codereview.SetReviewer(ctxt, clnum, who); err != nil {
			fmt.Fprintf(w, "ERROR: setting reviewer: %v", err)
			return
		}
		var cl codereview.CL
		if err := app.ReadData(ctxt, "CL", clnum, &cl); err != nil {
			fmt.Fprintf(w, "ERROR: refreshing CL: %v", err)
			return
		}
		fmt.Fprintf(w, "%s", d.short(d.reviewer(&cl)))
		return
	}
}
