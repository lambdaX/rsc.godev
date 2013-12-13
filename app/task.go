// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package app

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"appengine"
	"appengine/datastore"
	"appengine/taskqueue"
)

var scan = struct {
	sync.RWMutex
	m map[string]func(appengine.Context, string, string) error
}{
	m: make(map[string]func(appengine.Context, string, string) error),
}

// Scan registers a datastore trigger function.
// Periodically, the app will scan the datastore for records matching
// the query q, and for each such record will create a task
// to run the function f.
func ScanData(name string, period time.Duration, q *datastore.Query, f func(ctxt appengine.Context, kind, key string) error) {
	scan.Lock()
	defer scan.Unlock()
	if scan.m[name] != nil {
		panic("app.ScanData: multiple registrations for name: " + name)
	}
	scan.m[name] = f
	Cron("app.scan."+name, period, func(ctxt appengine.Context) error {
		return scanData(ctxt, name, period, q, f)
	})
}

// The only time we make a cron task retry is if the cron function
// reports ErrMoreCron, meaning it wants more work. In that case,
// we want the retry to happen quickly. Especially if this happens
// multiple times, we don't want the backoff to grow to something huge.
// We impose a retry limit of 1000 retries to avoid true runaways.
var scanRetryOptions = &taskqueue.RetryOptions{
	RetryLimit: 10,
	MinBackoff: 1 * time.Second,
	MaxBackoff: 1 * time.Hour,
}

func scanData(ctxt appengine.Context, name string, period time.Duration, q *datastore.Query, f func(ctxt appengine.Context, kind, key string) error) error {
	// TODO: Handle even more keys by using cursor.
	const chunk = 100000

	keys, err := q.Limit(chunk).KeysOnly().GetAll(ctxt, nil)
	if err != nil {
		ctxt.Errorf("scandata %q: %v", name, err)
		return nil
	}

	const maxBatch = 100
	var batch []*taskqueue.Task
	for _, key := range keys {
		kind := key.Kind()
		id := key.StringID()
		t := taskqueue.NewPOSTTask("/admin/app/scandata", url.Values{"name": {name}, "kind": {kind}, "key": {id}})
		t.Name = strings.Replace("app.scandata."+kind+"."+id, ".", "_", -1)
		t.RetryOptions = scanRetryOptions
		batch = append(batch, t)
		if len(batch) >= maxBatch {
			if _, err := taskqueue.AddMulti(ctxt, batch, "scandata"); err != nil {
				ctxt.Errorf("taskqueue.AddMulti scandata %q: %v", name, err)
			}
			batch = nil
		}
	}
	if len(batch) > 0 {
		if _, err := taskqueue.AddMulti(ctxt, batch, "scandata"); err != nil {
			ctxt.Errorf("taskqueue.AddMulti scandata %q: %v", name, err)
		}
	}
	return nil
}

func init() {
	http.HandleFunc("/admin/app/scandata", scanDataExec)
}

func scanDataExec(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	ctxt := appengine.NewContext(req)

	req.ParseForm()
	name := req.FormValue("name")
	kind := req.FormValue("kind")
	key := req.FormValue("key")
	scan.RLock()
	f := scan.m[name]
	scan.RUnlock()

	ctxt.Infof("scanData %q %q %q", name, kind, key)

	if f == nil || name == "" || kind == "" || key == "" {
		fmt.Fprintf(w, "missing url parameters\n")
		return
	}

	// Since we are being invoked using the task queue and we give each
	// task a name derived from the name,kind,key triple, there should only ever be
	// a single instance of scanDataExec for a given name,kind,key running at a time,
	// across all app instances. However, an admin might hit the URL
	// to force something, and we don't want to conflict with that, so
	// use a lease anyway.
	lock := fmt.Sprintf("app.scandata.%s.%s.%s", name, kind, key)
	if !Lock(ctxt, lock, 15*time.Minute) {
		// Report the error but do not use an error HTTP code.
		// We do not want the task to be retried, since some other task
		// is already taking care of it for us.
		// (Or if not, eventually the lease will expire and the next
		// invocation will run this for us.)
		fmt.Fprintf(w, "scandata %q: already locked\n", lock)
		return
	}
	defer Unlock(ctxt, lock)

	if err := f(ctxt, kind, key); err != nil {
		ctxt.Errorf("scandata %q: %v", lock, err)
		fmt.Fprintf(w, "scandata %q: %v\n", lock, err)
		return
	}

	fmt.Fprintf(w, "OK\n")
}
