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
// Apps using cron must add a definition for the cron queue to queue.yaml, like:
//
//	queue:
//	- name: cron
//	  rate: 10/s
//
// Apps using cron must also add a master app engine cron job to cron.yaml, like:
//
//	cron:
//	- description: app master cron
//	  url: /admin/app/cron
//	  schedule: every 1 minutes
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
var cronRetry = &taskqueue.RetryOptions{
	RetryLimit: 1000,
	MinBackoff: 1 * time.Second,
	MaxBackoff: 10 * time.Second,
}

func init() {
	TaskFunc("cron", cronExec, "cron", cronRetry)
}

// cronHandler is called by app engine cron to check for work
// and also called by task queue invocations to run the work for
// a specific registered functions.
func cronHandler(w http.ResponseWriter, req *http.Request) {
	cron.RLock()
	list := cron.list
	cron.RUnlock()

	ctxt := appengine.NewContext(req)
	force := req.FormValue("force") == "1"

	// We're being called by app engine master cron,
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

	for _, cr := range list {
		if now.Round(cr.dt) != old.Round(cr.dt) || force {
			ctxt.Infof("start cron %s", cr.name)
			Task(ctxt, "app.cron."+cr.name, "cron", cr.name)
		}
	}
}

// cronExec runs the cron job for the given entry.
func cronExec(ctxt appengine.Context, name string) error {
	cron.RLock()
	list := cron.list
	cron.RUnlock()

	var cr cronEntry
	for _, cr = range list {
		if cr.name == name {
			goto Found
		}
	}
	ctxt.Errorf("unknown cron entry %q", name)
	return nil

Found:
	if err := cr.f(ctxt); err != nil {
		if err == ErrMoreCron {
			// The cron job found that it had more work than it could do
			// and wants to run again. Arrange this by making the task
			// seem to fail.
			ctxt.Infof("cron job %q: ErrMoreCron", cr.name)
			return fmt.Errorf("cron job %q wants to run some more", cr.name)
		}
		// The cron job failed, but there's no reason to think running it again
		// right now will help. Let this instance finish successfully.
		// It will run again at the next scheduled time.
		ctxt.Infof("cron job %q: %v", cr.name, err)
		return nil
	}
	return nil
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
