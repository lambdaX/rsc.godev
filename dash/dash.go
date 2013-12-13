// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dash

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"codereview"
	"issue"

	"appengine"
	"appengine/datastore"
)

func init() {
	http.HandleFunc("/", showDash)
	http.HandleFunc("/issue", showIssueDash)
}

type Group struct {
	Dir   string
	Items []*Item
}

type Item struct {
	Bug *issue.Issue
	CLs []*codereview.CL
}

func showDash(w http.ResponseWriter, req *http.Request) {
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

	var bugs []*issue.Issue
	_, err = datastore.NewQuery("Issue").
		Filter("State =", "open").
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
		g := groups[dir]
		if g == nil {
			g = &Group{Dir: dir}
			groups[dir] = g
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

	t, err := template.New("main").Funcs(template.FuncMap{
		"clStatus": clStatus,
	}).Parse(`<html>
<head>
<style>
</style>
</head>
<body>
<h1>Go 1.3 issues and code reviews</h1>
<table>
{{range .}}
	<tr><td colspan=5><b>{{.Dir}}</b>
	{{range .Items}}
		{{if .Bug}}
			{{with .Bug}}
				<tr><td>&nbsp; &nbsp; &nbsp; &nbsp; <td><a href="https://code.google.com/p/go/issues/detail?id={{.ID}}">issue {{.ID}}</a><td>{{.Owner}}<td>{{.Summary}}
			{{end}}
			{{range .CLs}}
				<tr><td>&nbsp; &nbsp; &nbsp; &nbsp; <td>&nbsp; &nbsp; <a href="https://codereview.appspot.com/{{.CL}}">CL {{.CL}}</a><td>&nbsp; &nbsp; {{.Owner}}<td>&nbsp; &nbsp; {{.Summary}}
				<tr><td><td><td><td>&nbsp; &nbsp; &nbsp; &nbsp; <font size=-1>{{clStatus .}}</font>
			{{end}}
		{{else}}
			{{range .CLs}}
				<tr><td>&nbsp; &nbsp; &nbsp; &nbsp; <td><a href="https://codereview.appspot.com/{{.CL}}">CL {{.CL}}</a><td>{{.Owner}}<td>{{.Summary}}
				<tr><td><td><td><td>&nbsp; &nbsp; <font size=-1 style="font-family: sans-serif">{{clStatus .}}</font>
			{{end}}
		{{end}}
	{{end}}
{{end}}
</table>
</body>
	`)
	if err != nil {
		ctxt.Errorf("parsing template: %v", err)
		return
	}

	if err := t.Execute(w, groups); err != nil {
		ctxt.Errorf("execute: %v", err)
		return
	}
}

var lgtmRE = regexp.MustCompile(`(?im)^LGTM`)
var notlgtmRE = regexp.MustCompile(`(?im)^NOT LGTM`)

// LGTM: r / NOT LGTM: rsc / last update: XXX by XXX
func clStatus(cl *codereview.CL) string {
	lgtm := make(map[string]bool)
	for _, m := range cl.Messages {
		if notlgtmRE.MatchString(m.Text) {
			lgtm[m.Sender] = false
		} else if lgtmRE.MatchString(m.Text) {
			lgtm[m.Sender] = true
		}
	}
	var who []string
	for key := range lgtm {
		who = append(who, key)
	}
	sort.Strings(who)

	var w bytes.Buffer
	n := 0
	for _, k := range who {
		if lgtm[k] {
			if n == 0 {
				fmt.Fprintf(&w, "LGTM: ")
			} else {
				fmt.Fprintf(&w, ", ")
			}
			fmt.Fprintf(&w, "%s", shorten(k))
			n++
		}
	}
	n2 := 0
	for _, k := range who {
		if x, ok := lgtm[k]; ok && !x {
			if n2 == 0 {
				if n > 0 {
					fmt.Fprintf(&w, " / ")
				}
				fmt.Fprintf(&w, "NOT LGTM: ")
			} else {
				fmt.Fprintf(&w, ", ")
			}
			fmt.Fprintf(&w, "%s", shorten(k))
			n2++
		}
	}
	if n > 0 || n2 > 0 {
		fmt.Fprintf(&w, " / ")
	}
	if len(cl.Messages) > 0 {
		m := cl.Messages[len(cl.Messages)-1]
		fmt.Fprintf(&w, "last update: %v by %v [%s]", m.Time.Local().Format("2006-01-02 15:04:05"), shorten(m.Sender), firstLine(m.Text))
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

func itemDir(item *Item) string {
	if item.Bug != nil {
		if dir := descDir(item.Bug.Summary); dir != "" {
			return dir
		}
		return "?"
	}
	for _, cl := range item.CLs {
		if dir := descDir(cl.Summary); dir != "" {
			return dir
		}
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

func showIssueDash(w http.ResponseWriter, req *http.Request) {
	fmt.Fprintf(w, "<html><pre>\n")
	const chunk = 500
	ctxt := appengine.NewContext(req)
	q := datastore.NewQuery("Issue").Limit(chunk)
	//q = q.Filter("Label =", "Release-Go1.3")
	q = q.Order("Summary")

	if cursor := req.FormValue("cursor"); cursor != "" {
		cur, err := datastore.DecodeCursor(cursor)
		if err == nil {
			q = q.Start(cur)
		}
	}

	fmt.Fprintf(w, "<table>\n")
	n := 0
	it := q.Run(ctxt)
	for {
		var bug issue.Issue
		_, err := it.Next(&bug)
		if err != nil {
			break
		}
		fmt.Fprintf(w, "<tr><td><a href=\"http://golang.org/issue/%v\">%v</a><td>%v<td>%s\n", bug.ID, bug.ID, bug.Owner, bug.Summary)
		n++
	}
	fmt.Fprintf(w, "</table>\n")

	if n == chunk {
		cur, err := it.Cursor()
		if err == nil {
			fmt.Fprintf(w, "<a href=\"/issue?cursor=%s\">more</a>\n", cur)
		}
	}
}
