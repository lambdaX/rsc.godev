// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package app

import (
	"bytes"
	"fmt"
	"net/http"
	"sync"

	"appengine"
)

type statusElem struct {
	heading string
	f       func(appengine.Context) string
}

var status struct {
	sync.RWMutex
	elems []statusElem
}

// RegisterStatus add a new section to the status page.
// The section has the given fixed heading and HTML body obtained by
// calling content(ctxt).
//
// The status page is served as /admin/app/status.
//
func RegisterStatus(heading string, content func(ctxt appengine.Context) string) {
	status.Lock()
	status.elems = append(status.elems, statusElem{heading, content})
	status.Unlock()
}

// StatusPage serves an HTTP request by printing a server status page
// containing the status elements registered with RegisterStatus.
//
// StatusPage is automatically registered to serve /admin/app/status.
// It is exported so that clients can register it on other URLs as well.
// For example, if the status page should be made publicly visible:
//
//	func init() {
//		http.HandleFunc("/status", app.StatusPage)
//	}
//
func StatusPage(w http.ResponseWriter, req *http.Request) {
	status.RLock()
	elems := status.elems
	status.RUnlock()

	ctxt := appengine.NewContext(req)
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "<h1>status</h2>\n")
	for _, elem := range elems {
		fmt.Fprintf(&buf, "<h2>%s</h2>\n", elem.heading)
		buf.WriteString(elem.f(ctxt))
	}

	w.Write(buf.Bytes())
}

func init() {
	http.HandleFunc("/admin/app/status", StatusPage)
}
