// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rietveld

import (
	"errors"
)

// This was copied from package exp/terminal

// ReadPassword reads a line of input from a terminal without local echo.  This
// is commonly used for inputting passwords and other sensitive data. The slice
// returned does not include the \n.
func readPassword(fd uintptr) ([]byte, error) {
	return nil, errors.New("gone")
}
