// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package codereview

import (
	"bytes"
	"encoding/xml"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	
	"app"

	"appengine"
	"appengine/urlfetch"
	"code.google.com/p/goauth2/oauth"
)

func init() {
	http.HandleFunc("/admin/codelogin", codelogin)
	http.HandleFunc("/codetoken", codetoken)
}

func oauthConfig(ctxt appengine.Context) (*oauth.Config, error) {
	var clientID, clientSecret string

	if err := app.ReadMeta(ctxt, "googleapi.clientid", &clientID); err != nil {
		return nil, err
	}
	if err := app.ReadMeta(ctxt, "googleapi.clientsecret", &clientSecret); err != nil {
		return nil, err
	}
	
	cfg := &oauth.Config{
		ClientId: clientID,
		ClientSecret: clientSecret,
		Scope: "https://code.google.com/feeds/issues",
		AuthURL:  "https://accounts.google.com/o/oauth2/auth",
		TokenURL: "https://accounts.google.com/o/oauth2/token",
		RedirectURL: "https://go-dev.appspot.com/codetoken",
		AccessType: "offline",
	}
	return cfg, nil
}

func codelogin(w http.ResponseWriter, req *http.Request) {
	ctxt := appengine.NewContext(req)
	randState, err := randomID()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	
	clientID := req.FormValue("clientid")
	if clientID != "" {
		app.WriteMeta(ctxt, "googleapi.clientid", &clientID)
	}
	clientSecret := req.FormValue("clientsecret")
	if clientSecret != "" {
		app.WriteMeta(ctxt, "googleapi.clientsecret", &clientSecret)
	}

	if err := app.WriteMeta(ctxt, "codelogin.random", &randState); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	cfg, err := oauthConfig(ctxt)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	
	authURL := cfg.AuthCodeURL(randState)
	http.Redirect(w, req, authURL, 301)
}

func codetoken(w http.ResponseWriter, req *http.Request) {
	ctxt := appengine.NewContext(req)
	
	var randState string
	if err := app.ReadMeta(ctxt, "codelogin.random", &randState); err != nil {
		panic(err)
	}
	
	cfg, err := oauthConfig(ctxt)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	
	if req.FormValue("state") != randState {
		http.Error(w, "bad state", 500)
		return
	}
	code := req.FormValue("code")
	if code == "" {
		http.Error(w, "missing code", 500)
		return
	}
	
	tr := &oauth.Transport{
		Config: cfg,
		Transport: urlfetch.Client(ctxt).Transport,
	}

	_, err = tr.Exchange(code)
	if err != nil {
		http.Error(w, "exchanging code: " + err.Error(), 500)
		return
	}
	
	if err := app.WriteMeta(ctxt, "codelogin.token", tr.Token); err != nil {
		http.Error(w, "writing token: " + err.Error(), 500)
		return
	}
	
	app.DeleteMeta(ctxt, "codelogin.random")
	
	fmt.Fprintf(w, "have token; expires at %v\n", tr.Token.Expiry)
}
	
func randomID() (string, error) {
	buf := make([]byte, 16)
	_, err := io.ReadFull(rand.Reader, buf)
	if err != nil {
		return "", fmt.Errorf("RandomID: reading rand.Reader: %v", err)
	}
	return fmt.Sprintf("%x", buf), nil
}

func init() {
	http.HandleFunc("/admin/testissue", testIssue)
}

func testIssue(w http.ResponseWriter, req *http.Request) {
	err := postIssueComment(appengine.NewContext(req), "7737", "comment!")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	fmt.Fprintf(w, "OK!\n")
}

func postIssueComment(ctxt appengine.Context, id, comment string) error{
	cfg, err := oauthConfig(ctxt)
	if err != nil {
		return fmt.Errorf("oauthconfig: %v", err)
	}
	
	var tok oauth.Token
	if err := app.ReadMeta(ctxt, "codelogin.token", &tok); err != nil {
		return fmt.Errorf("reading token: %v", err)
	}
	
	tr := &oauth.Transport{
		Config: cfg,
		Token: &tok,
		Transport: urlfetch.Client(ctxt).Transport,
	}
	client := tr.Client()

	var buf bytes.Buffer
	buf.WriteString(`<?xml version='1.0' encoding='UTF-8'?>
<entry xmlns='http://www.w3.org/2005/Atom' xmlns:issues='http://schemas.google.com/projecthosting/issues/2009'>
  <content type='html'>`)
	xml.Escape(&buf, []byte(comment))
	buf.WriteString(`</content>
  <author>
    <name>ignored</name>
  </author>
  <issues:updates>
  </issues:updates>
</entry>
`)
	u := "https://code.google.com/feeds/issues/p/go/issues/"+id+"/comments/full"
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
	return nil
}
