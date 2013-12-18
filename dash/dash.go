// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dash

import (
	"bytes"
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
)

func init() {
	http.HandleFunc("/", showDash)
	http.HandleFunc("/uiop", uiOperation)
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

func shortEmail(s interface{}) interface{} {
	switch s := s.(type) {
	case string:
		if i := strings.Index(s, "@"); i >= 0 {
			return s[:i]
		}
		return s
	case []string:
		v := make([]string, len(s))
		for i, t := range s {
			v[i] = shortEmail(t).(string)
		}
		return v
	default:
		return s
	}
	return s
}

func oldTime(t time.Time) bool {
	return time.Since(t) > 7*24*time.Hour
}

func comma(s []string) string {
	return strings.Join(s, ",")
}

func space(s []string) string {
	return strings.Join(s, " ")
}

func since(t time.Time) string {
	dt := time.Since(t)
	return fmt.Sprintf("%.1f days ago", float64(dt)/float64(24*time.Hour))
}

func findSelf(ctxt appengine.Context) string {
	self := ""
	u := user.Current(ctxt)
	if u != nil {
		self = codereview.IsReviewer(u.Email)
	}
	return self
}

func showDash(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/login" {
		http.Redirect(w, req, "/", 302)
		return
	}
	if req.URL.Path != "/" {
		http.ServeFile(w, req, "static/"+req.URL.Path)
		return
	}
	const chunk = 1000
	ctxt := appengine.NewContext(req)
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

	// There are some CLs in datastore that are not active but
	// have not been updated to DV 6 yet and still say Active=true
	// in the datastore index; filter those out.
	out := cls[:0]
	for _, cl := range cls {
		if cl.Active {
			out = append(out, cl)
		}
	}
	cls = out

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

	// TODO: Rewrite.

	nrow := 0
	self := findSelf(ctxt)
	isme := func(s string) bool { return s == self || s == "golang-dev" }
	defaultReviewer := func(cl *codereview.CL) string {
		if cl.PrimaryReviewer == "" {
			return "golang-dev"
		}
		return cl.PrimaryReviewer
	}

	var pref UserPref
	if self != "" {
		app.ReadData(ctxt, "UserPref", self, &pref)
	}
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

	tmpl, err := ioutil.ReadFile("template/dash.html")
	if err != nil {
		ctxt.Errorf("reading template: %v", err)
		return
	}
	t, err := template.New("main").Funcs(template.FuncMap{
		"clStatus": clStatus,
		"short":    shortEmail,
		"old":      oldTime,
		"since":    since,
		"comma":    comma,
		"space":    space,
		"resetAlt": func() string { nrow = 0; return "" },
		"nextAlt":  func() string { nrow++; return "" },
		"altColor": func() string {
			if nrow == 0 {
				return "first"
			}
			return "second"
		},
		"isme":            isme,
		"defaultReviewer": defaultReviewer,
		"todo":            todo,
		"todoItem":        todoItem,
		"todoGroup":       todoGroup,
		"muted":           muted,
	}).Parse(string(tmpl))
	if err != nil {
		ctxt.Errorf("parsing template: %v", err)
		return
	}

	data := struct {
		User string
		Dirs map[string]*Group
	}{
		self,
		groups,
	}

	if err := t.Execute(w, data); err != nil {
		ctxt.Errorf("execute: %v", err)
		fmt.Fprintf(w, "error executing template\n")
		return
	}
}

var lgtmRE = regexp.MustCompile(`(?im)^LGTM`)
var notlgtmRE = regexp.MustCompile(`(?im)^NOT LGTM`)

// LGTM: r / NOT LGTM: rsc / last update: XXX by XXX
func clStatus(cl *codereview.CL) string {
	var w bytes.Buffer
	fmt.Fprintf(&w, "LGTM: %v / NOT LGTM: %v / R=%v", cl.LGTM, cl.NOTLGTM, cl.PrimaryReviewer)
	fmt.Fprintf(&w, "delta %d", cl.Delta)
	fmt.Fprintf(&w, " / repo %s", cl.Repo)
	fmt.Fprintf(&w, " / dirs %v", cl.Dirs())
	if len(cl.Messages) > 0 {
		m := cl.Messages[len(cl.Messages)-1]
		fmt.Fprintf(&w, " / last update: %v by %v [%s]", m.Time.Local().Format("2006-01-02 15:04:05"), shorten(m.Sender), firstLine(m.Text))
	}

	return w.String()
}

func shorten(email string) string {
	if i := strings.Index(email, "@"); i >= 0 {
		email = email[:i]
	}
	return email
}

func firstLine(t string) string {
	i := strings.Index(t, "\n")
	if i >= 0 {
		t = t[:i]
	}
	if len(t) > 50 {
		t = t[:50] + "..."
	}
	return t
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

func uiOperation(w http.ResponseWriter, req *http.Request) {
	ctxt := appengine.NewContext(req)
	self := findSelf(ctxt)
	if self == "" {
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
			app.ReadData(ctxt, "UserPref", self, &pref)
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
			return app.WriteData(ctxt, "UserPref", self, &pref)
		})
		if err != nil {
			w.WriteHeader(501)
			fmt.Fprintf(w, "unable to update")
			return
		}
	}
}

type UserPref struct {
	Muted []string
}
