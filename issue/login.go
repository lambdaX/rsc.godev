// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package issue

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"app"

	"code.google.com/p/goauth2/oauth"
	"github.com/rsc/appstats"

	"appengine"
	"appengine/datastore"
	"appengine/urlfetch"
)

func oauthConfig(ctxt appengine.Context) (*oauth.Config, error) {
	var clientID, clientSecret string

	if err := app.ReadMeta(ctxt, "googleapi.clientid", &clientID); err != nil {
		return nil, err
	}
	if err := app.ReadMeta(ctxt, "googleapi.clientsecret", &clientSecret); err != nil {
		return nil, err
	}

	cfg := &oauth.Config{
		ClientId:     clientID,
		ClientSecret: clientSecret,
		Scope:        "https://code.google.com/feeds/issues",
		AuthURL:      "https://accounts.google.com/o/oauth2/auth",
		TokenURL:     "https://accounts.google.com/o/oauth2/token",
		RedirectURL:  "https://go-dev.appspot.com/codetoken",
		AccessType:   "offline",
	}
	return cfg, nil
}

func init() {
	http.Handle("/admin/testclose/", appstats.NewHandler(testIssue))
	http.Handle("/admin/testmove", appstats.NewHandler(doMoves))

	return
	app.ScanData("issue.github1", 15*time.Minute,
		datastore.NewQuery("Issue").Filter("NeedGithubNote =", true),
		postMovedNote)
}

func testIssue(ctxt appengine.Context, w http.ResponseWriter, req *http.Request) {
	err := postMovedNote(ctxt, "Issue", strings.TrimPrefix(req.URL.Path, "/admin/testclose/"))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	fmt.Fprintf(w, "OK!\n")
}

func doMoves(ctxt appengine.Context, w http.ResponseWriter, req *http.Request) {
	q := datastore.NewQuery("Issue").Filter("NeedGithubNote =", true).
		Limit(10)
	it := q.Run(ctxt)
	for {
		var old Issue
		_, err := it.Next(&old)
		if err != nil {
			break
		}
		fmt.Fprintf(w, "%s\n", fmt.Sprint(old.ID))
		if err := postMovedNote(ctxt, "Issue", fmt.Sprint(old.ID)); err != nil {
			fmt.Fprintf(w, "\t%s\n", err)
		}
	}
}

func postMovedNote(ctxt appengine.Context, kind, id string) error {
	var old Issue
	if err := app.ReadData(ctxt, "Issue", id, &old); err != nil {
		return err
	}
	updateIssue(&old)
	if !old.NeedGithubNote {
		err := app.Transaction(ctxt, func(ctxt appengine.Context) error {
			var old Issue
			if err := app.ReadData(ctxt, "Issue", id, &old); err != nil {
				return err
			}
			old.NeedGithubNote = false
			return app.WriteData(ctxt, "Issue", id, &old)
		})
		return err
	}

	cfg, err := oauthConfig(ctxt)
	if err != nil {
		return fmt.Errorf("oauthconfig: %v", err)
	}

	var tok oauth.Token
	if err := app.ReadMeta(ctxt, "codelogin.token", &tok); err != nil {
		return fmt.Errorf("reading token: %v", err)
	}

	tr := &oauth.Transport{
		Config:    cfg,
		Token:     &tok,
		Transport: &urlfetch.Transport{Context: ctxt, Deadline: 45 * time.Second},
	}
	client := tr.Client()

	status := ""
	if old.State != "closed" {
		status = "<issues:status>Moved</issues:status>"
	}

	var buf bytes.Buffer
	buf.WriteString(`<?xml version='1.0' encoding='UTF-8'?>
<entry xmlns='http://www.w3.org/2005/Atom' xmlns:issues='http://schemas.google.com/projecthosting/issues/2009'>
  <content type='html'>`)
	xml.Escape(&buf, []byte(fmt.Sprintf("This issue has moved to https://golang.org/issue/%s\n", id)))
	buf.WriteString(`</content>
  <author>
    <name>ignored</name>
  </author>
  <issues:sendEmail>False</issues:sendEmail>
  <issues:updates>
    <issues:label>IssueMoved</issues:label>
    <issues:label>Restrict-AddIssueComment-Commit</issues:label>
    ` + status + `
  </issues:updates>
</entry>
`)
	u := "https://code.google.com/feeds/issues/p/go/issues/" + id + "/comments/full"
	req, err := http.NewRequest("POST", u, &buf)
	if err != nil {
		return fmt.Errorf("write: %v", err)
	}
	req.Header.Set("Content-Type", "application/atom+xml")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("write: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		buf.Reset()
		io.Copy(&buf, resp.Body)
		return fmt.Errorf("write: %v\n%s", resp.Status, buf.String())
	}

	err = app.Transaction(ctxt, func(ctxt appengine.Context) error {
		var old Issue
		if err := app.ReadData(ctxt, "Issue", id, &old); err != nil {
			return err
		}
		old.NeedGithubNote = false
		old.Label = append(old.Label, "IssueMoved", "Restrict-AddIssueComment-Commit")
		return app.WriteData(ctxt, "Issue", id, &old)
	})
	return err
}
