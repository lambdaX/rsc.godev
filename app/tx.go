// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package app

import (
	"appengine"
	"appengine/datastore"
)

// Transaction executes f in a transaction.
// If an error occurs, Transaction returns it but also logs it using ctxt.Errorf.
// All transactions are marked as "cross-group" (there is no harm in doing so).
func Transaction(ctxt appengine.Context, f func(ctxt appengine.Context) error) error {
	err := datastore.RunInTransaction(ctxt, f, &datastore.TransactionOptions{XG: true})
	if err != nil {
		ctxt.Errorf("transaction failed: %v", err)
	}
	return err
}
