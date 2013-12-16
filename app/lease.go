// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package app

import (
	"errors"
	"net/http"
	"time"

	"appengine"
	"appengine/datastore"
)

var errLocked = errors.New("locked")

// Lock and Unlock implement advisory lease-based locking using datastore records.
//
// Lock attempts to acquire a lock with the given name.
// If unsuccessful, Lock returns false.
// If successful, no other call to Lock will succeed until the duration dt has elapsed
// or Unlock has been called with the same name.
func Lock(ctxt appengine.Context, name string, dt time.Duration) bool {
	now := time.Now()
	err := Transaction(ctxt, func(ctxt appengine.Context) error {
		var t time.Time
		if err := ReadMeta(ctxt, "Lock:"+name, &t); err != nil && err != datastore.ErrNoSuchEntity {
			return err
		}
		if now.Before(t) {
			ctxt.Infof("Lock %s: locked until %v", name, t)
			return errLocked
		}
		return WriteMeta(ctxt, "Lock:"+name, now.Add(dt))
	})
	return err == nil
}

// Lock and Unlock implement advisory lease-based locking using datastore records.
//
// Unlock releases a lock acquired by Lock.
func Unlock(ctxt appengine.Context, name string) {
	DeleteMeta(ctxt, "Lock:"+name)
}

func breaklock(w http.ResponseWriter, req *http.Request) {
	ctxt := appengine.NewContext(req)
	Unlock(ctxt, req.URL.Query().Get("name"))
}

func init() {
	http.HandleFunc("/admin/app/breaklock", breaklock)
}
