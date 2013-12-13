// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package app

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"appengine"
	"appengine/datastore"
	"appengine/taskqueue"
)

var cron struct {
	sync.RWMutex
	list []cronEntry
}

type cronEntry struct {
	name string
	dt   time.Duration
	f    func(appengine.Context) error
}

// Cron registers a function to call once per period.
// Each call to Cron must use a unique name; Cron uses that name internally
// to identify the work being done.
//
// Apps using cron must add a definition for the cron queue to queue.yaml:
//
//	XXX
//
// Apps using cron must also add a master app engine cron job to cron.yaml:
//
//	XXX
//
func Cron(name string, period time.Duration, f func(appengine.Context) error) {
	cron.Lock()
	defer cron.Unlock()
	for _, cr := range cron.list {
		if cr.name == name {
			panic("app.Cron: multiple registrations for " + name)
		}
	}
	cron.list = append(cron.list, cronEntry{name, period, f})
}

// If a function registered and run by Cron returns ErrMoreWork,
// then it is rerun as soon as possible instead of waiting until the next
// period has elapsed.
var ErrMoreCron = errors.New("cron job has more work to do")

func init() {
	http.HandleFunc("/admin/app/cron", cronHandler)
	RegisterStatus("cron", cronStatus)
}

// The only time we make a cron task retry is if the cron function
// reports ErrMoreCron, meaning it wants more work. In that case,
// we want the retry to happen quickly. Especially if this happens
// multiple times, we don't want the backoff to grow to something huge.
// We impose a retry limit of 1000 retries to avoid true runaways.
var cronRetryOptions = &taskqueue.RetryOptions{
	RetryLimit: 1000,
	MinBackoff: 1 * time.Second,
	MaxBackoff: 10 * time.Second,
}

// cronHandler is called by app engine cron to check for work
// and also called by task queue invocations to run the work for
// a specific registered functions.
func cronHandler(w http.ResponseWriter, req *http.Request) {
	cron.RLock()
	list := cron.list
	cron.RUnlock()

	ctxt := appengine.NewContext(req)

	// Are we being called to execute a specific cron job?
	req.ParseForm()
	if name := req.FormValue("name"); name != "" {
		for _, cr := range list {
			if cr.name == name {
				cronExec(ctxt, w, req, cr)
				return
			}
		}
		ctxt.Errorf("unknown cron name %q", name)
		return
	}
	force := req.FormValue("force") == "1"

	// Otherwise, we're being called by app engine master cron,
	// so look for new work to queue in tasks.
	now := time.Now()
	var old time.Time
	err := Transaction(ctxt, func(ctxt appengine.Context) error {
		if err := ReadMeta(ctxt, "app.cron.time", &old); err != nil && err != datastore.ErrNoSuchEntity {
			return err
		}
		if !old.Before(now) {
			return nil
		}
		return WriteMeta(ctxt, "app.cron.time", now)
	})
	if err != nil { // already logged
		return
	}

	ctxt.Infof("cron %v -> %v", old, now)

	var batch []*taskqueue.Task
	const maxBatch = 100
	for _, cr := range list {
		if now.Round(cr.dt) != old.Round(cr.dt) || force {
			ctxt.Infof("start cron %s", cr.name)
			t := taskqueue.NewPOSTTask("/admin/app/cron", url.Values{"name": {cr.name}})
			t.Name = strings.Replace("app.cron."+cr.name, ".", "_", -1)
			t.RetryOptions = cronRetryOptions
			batch = append(batch, t)
			if len(batch) >= maxBatch {
				if _, err := taskqueue.AddMulti(ctxt, batch, "cron"); err != nil {
					ctxt.Errorf("taskqueue.AddMulti cron: %v", err)
				}
				batch = nil
			}
		}
	}
	if len(batch) > 0 {
		if _, err := taskqueue.AddMulti(ctxt, batch, "cron"); err != nil {
			ctxt.Errorf("taskqueue.AddMulti cron: %v", err)
		}
	}
}

// cronExec runs the cron job for the given entry.
func cronExec(ctxt appengine.Context, w http.ResponseWriter, req *http.Request, cr cronEntry) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	// Since we are being invoked using the task queue and we give each
	// task a name derived from the cronEntry, there should only ever be
	// a single instance of cronExec for a given cr running at a time,
	// across all app instances. However, an admin might hit the URL
	// to force something, and we don't want to conflict with that, so
	// use a lease anyway.
	if !Lock(ctxt, "app.cron."+cr.name, 15*time.Minute) {
		// Report the error but do not use an error HTTP code.
		// We do not want the task to be retried, since some other task
		// is already taking care of it for us.
		// (Or if not, eventually the lease will expire and the next cron
		// invocation will run this for us.)
		fmt.Fprintf(w, "cron job %q: already locked\n", cr.name)
		return
	}
	defer Unlock(ctxt, "app.cron."+cr.name)

	if err := cr.f(ctxt); err != nil {
		if err == ErrMoreCron {
			// The cron job found that it had more work than it could do
			// and wants to run again. Arrange this by making the task
			// seem to fail.
			w.WriteHeader(409)
			fmt.Fprintf(w, "cron job %q wants to run some more\n", cr.name)
			return
		}
		// The cron job failed, but there's no reason to think running it again
		// right now will help. Let this instance finish successfully.
		// It will run again at the next scheduled time.
		fmt.Fprintf(w, "cron job %q: %v\n", cr.name, err)
		return
	}

	fmt.Fprintf(w, "OK\n")
}

func cronStatus(ctxt appengine.Context) string {
	cron.RLock()
	list := cron.list
	cron.RUnlock()
	var t time.Time
	ReadMeta(ctxt, "app.cron.time", &t)

	w := new(bytes.Buffer)

	fmt.Fprintf(w, "cron tasks last started at %s\n", t)
	for _, cr := range list {
		fmt.Fprintf(w, "\t%v every %v\n", cr.name, cr.dt)
	}

	return "<pre>" + html.EscapeString(w.String()) + "</pre>\n"
}
