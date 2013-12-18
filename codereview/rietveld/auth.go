// goetveld - Go interface to the Rietveld core review server.
//
//   https://wiki.ubuntu.com/goetveld
//
// Copyright (c) 2011 Canonical Ltd.
//
// Written by Gustavo Niemeyer <gustavo.niemeyer@canonical.com>
//
// This software is licensed under the GNU Lesser General Public License
// version 3 (LGPLv3), with an additional exception relative to static
// linkage. See the LICENSE file for details.

package rietveld

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"appengine"

	http_without_bugs "code.google.com/p/rsc/codebot/http"
)

type Auth interface {
	// Sign changes req to introduce the necessary authentication tokens.
	// The returned time informs the moment in which the signature was made,
	// and serves as a good concurrency-free argument to provide to Login.
	Sign(rietveldURL string, req *http.Request) (when time.Time, err error)

	// Login authenticates onto the rietveld instance at rietveldURL.
	// If after is not the zero Time, the authentication instance may
	// do nothing and return in case it has already logged in after
	// the given timestamp.
	Login(rietveldURL string, after time.Time, t http.RoundTripper) error

	// Logout forgets any authentication information previously stored in
	// memory or cached elsewhere.
	Logout(rietveldURL string) error
}

type AuthUI interface {
	Credentials(loginURL, previousUser string) (user, passwd string, err error)
}

// ConsoleUI is an AuthUI that requests credentials interactively in
// the terminal.
var ConsoleUI AuthUI = &consoleUI{}

// DefaultAuth is an Auth that authenticates against the standard Google
// service used by codereview.appspot.com, caches the authentication HTTP
// cookies on disk, and requests credentials interactively in the terminal.
var DefaultAuth = NewAuth(ConsoleUI, true, "", nil)

// NewAuth returns an Auth that authenticates against the standard service
// for installed applications from Google.  This is used by the rietveld
// deployment made available at codereview.appspot.com.
//
// If cache is true, the authentication HTTP cookies will be cached
// on disk under ~/.goetveld_<rietveld host>.
//
// If the empty string is provided as loginURL, it defaults to the standard
// Google service at https://www.google.com/accounts/ClientLogin.
//
// For more details, see the documentation:
//
//   http://code.google.com/apis/accounts/AuthForInstalledApps.html
//
func NewAuth(ui AuthUI, cache bool, loginURL string, ctxt appengine.Context) Auth {
	if loginURL == "" {
		loginURL = "https://www.google.com/accounts/ClientLogin"
	}
	auth := &standardAuth{ui: ui, loginURL: loginURL, ctxt: ctxt}
	if cache {
		return &cachedAuth{auth}
	}
	return auth
}

type standardAuth struct {
	m         sync.RWMutex
	lastLogin time.Time
	user      string
	ui        AuthUI
	loginURL  string
	cookies   []*http.Cookie
	ctxt      appengine.Context
}

var redirectBlocked = errors.New("redirect blocked")

func (auth *standardAuth) Login(rietveldURL string, after time.Time, t http.RoundTripper) (err error) {
	client := &http_without_bugs.Client{
		CheckRedirect: func(r *http.Request, v []*http.Request) error { return redirectBlocked },
		Transport:     t,
	}
	auth.m.Lock()
	defer func() {
		auth.m.Unlock()
		if err != nil {
			logf("Login failed: %v", err)
		}
	}()

	if auth.lastLogin.After(after) {
		return nil
	}

	rurl, err := url.Parse(rietveldURL)
	if err != nil {
		return errors.New("invalid rietveld URL: " + rietveldURL)
	}

	accountType := "GOOGLE"
	if strings.HasSuffix(rurl.Host, ".google.com") {
		accountType = "HOSTED"
	}

	user, passwd, err := auth.ui.Credentials(auth.loginURL, auth.user)
	if err != nil {
		return err
	}
	auth.user = user

	loginForm := url.Values{
		"Email":       []string{user},
		"Passwd":      []string{passwd},
		"source":      []string{"goetveld"},
		"service":     []string{"ah"},
		"accountType": []string{accountType},
	}
	logf("Authenticating user %s...", user)
	r, err := client.PostForm(auth.loginURL, loginForm)
	if err != nil {
		return err
	}
	debugf("Authentication on %s returned: %s", auth.loginURL, r.Status)
	defer r.Body.Close()
	buf := bufio.NewReader(r.Body)
	args := make(map[string]string)
	for {
		line, prefix, err := buf.ReadLine()
		if prefix {
			return &LoginError{"ReadError", "line too long"}
		}
		if err != nil {
			if msg := args["Error"]; msg != "" {
				return &LoginError{msg, args["Info"]}
			}
			return &LoginError{"ReadError", err.Error()}
		}
		if len(line) == 0 {
			continue
		}
		debugf("ClientLogin response line: %s", line)
		kv := strings.SplitN(string(line), "=", 2)
		if len(kv) == 2 {
			args[kv[0]] = kv[1]
		}
		if kv[0] == "Auth" {
			break
		}
	}

	logf("Authorizing on %s...", rietveldURL)
	const marker = "http://example.com/marker"
	authForm := url.Values{
		"continue": []string{marker},
		"auth":     []string{args["Auth"]},
	}
	r, err = client.Get(rietveldURL + "/_ah/login?" + authForm.Encode())
	auth.ctxt.Infof("client.Get %v: r=%v, err=%v", rietveldURL+"/_ah/login?"+authForm.Encode(), r, err)
	if err == nil {
		r.Body.Close()
		return &LoginError{"AuthError", r.Status}
	} else if e, ok := err.(*url.Error); !ok || e.URL != marker {
		return &LoginError{"AuthError", err.Error()}
	}

	logf("Login on %s successful. %p", rietveldURL)
	auth.cookies = nil
	for _, cookie := range r.Cookies() {
		auth.cookies = append(auth.cookies, cookie)
	}
	auth.lastLogin = time.Now()
	return nil
}

func (auth *standardAuth) Logout(rietveldURL string) error {
	auth.m.Lock()
	auth.cookies = nil
	logf("Dropped in-memory authentication details.")
	auth.m.Unlock()
	return nil
}

func (auth *standardAuth) Sign(rietveldURL string, req *http.Request) (time.Time, error) {
	auth.m.RLock()
	defer auth.m.RUnlock()
	// Note that this _must_ be taken within the locked context, otherwise
	// there's a race when using this for the after argument of Login.
	when := time.Now()
	if len(auth.cookies) > 0 {
		debugf("Signing http request...")
		for _, cookie := range auth.cookies {
			debugf("Adding cookie: %s", cookie)
			req.AddCookie(cookie)
		}
	} else {
		debugf("No authentication information to sign http request.")
	}
	return when, nil
}

type cachedAuth struct {
	std *standardAuth
}

func (auth *cachedAuth) Login(rietveldURL string, after time.Time, t http.RoundTripper) error {
	err := auth.std.Login(rietveldURL, after, t)
	if err != nil {
		return err
	}
	err = auth.write(rietveldURL)
	if err != nil {
		logf("Error saving authentication details: %s", err)
	} else {
		logf("Saved authentication details.")
	}
	return err
}

func (auth *cachedAuth) Logout(rietveldURL string) error {
	path, err := auth.path(rietveldURL)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if filterNotFound(err) != nil {
		logf("Error removing cached authentication details: %s", err)
		auth.std.Logout(rietveldURL)
		return err
	} else {
		logf("Removed cached authentication details.")
	}
	return auth.std.Logout(rietveldURL)
}

func (auth *cachedAuth) Sign(rietveldURL string, req *http.Request) (time.Time, error) {
	auth.std.m.RLock()
	defer auth.std.m.RUnlock()
	if len(auth.std.cookies) == 0 {
		err := auth.read(rietveldURL)
		if err != nil {
			logf("Couldn't load cached authentication: %v", err)
			// Ignore the error. It's fine to not have the auth here.
			return time.Now(), nil
		} else {
			logf("Loaded cached authentication details.")
		}
	}
	return auth.std.Sign(rietveldURL, req)
}

func filterNotFound(err error) error {
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

type authDump struct {
	User     string
	LoginURL string
	Cookies  []*http.Cookie
}

func (auth *cachedAuth) path(rietveldURL string) (string, error) {
	rurl, err := url.Parse(rietveldURL)
	if err != nil {
		return "", err
	}
	return os.ExpandEnv("$HOME/.goetveld_" + rurl.Host), nil
}

func (auth *cachedAuth) read(rietveldURL string) error {
	path, err := auth.path(rietveldURL)
	if err != nil {
		return err
	}
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return filterNotFound(err)
	}
	dump := &authDump{}
	err = json.Unmarshal(data, dump)
	if err != nil {
		return err
	}
	auth.std.user = dump.User
	auth.std.loginURL = dump.LoginURL
	auth.std.cookies = dump.Cookies
	return nil
}

func (auth *cachedAuth) write(rietveldURL string) error {
	path, err := auth.path(rietveldURL)
	if err != nil {
		return err
	}
	data, err := json.Marshal(&authDump{auth.std.user, auth.std.loginURL, auth.std.cookies})
	if err != nil {
		return err
	}
	return ioutil.WriteFile(path, data, 0600)
}

type consoleUI struct{}

func wstderr(s ...string) {
	for i := range s {
		_, err := os.Stderr.Write([]byte(s[i]))
		if err != nil {
			panic(err)
		}
	}
}

func (c *consoleUI) Credentials(loginURL, previousUser string) (user, passwd string, err error) {
	host := loginURL
	if u, err := url.Parse(host); err == nil {
		host = u.Host
	}
	wstderr("Authenticating on ", host, "...\n")
	if previousUser == "" {
		wstderr("User: ")
	} else {
		wstderr("User [", previousUser, "]: ")
	}
	buf, err := bufio.NewReader(os.Stdin).ReadSlice('\n')
	if err != nil {
		return
	}
	user = string(buf[:len(buf)-1])
	wstderr("Password: ")
	buf, err = readPassword(os.Stdin.Fd())
	wstderr("\n")
	if err != nil {
		return "", "", err
	}
	passwd = string(buf)
	if user == "" {
		user = previousUser
		if user == "" {
			return "", "", errors.New("empty user")
		}
	}
	if passwd == "" {
		return "", "", errors.New("empty password")
	}
	return user, passwd, nil
}

type LoginError struct {
	Code string
	Info string
}

func (e *LoginError) Error() string {
	switch e.Code {
	// local errors
	case "ReadError":
		return "error reading response: " + e.Info
	case "AuthError":
		return "error authorizing on rietveld: " + e.Info

	// remote errors
	case "BadAuthentication":
		if e.Info == "InvalidSecondFactor" {
			return "application-specific password required"
		}
		return "invalid user or password"
	case "CaptchaRequired":
		return "captcha required; visit http://j.mp/unlock-captcha and retry"
	case "NotVerified":
		return "account not verified"
	case "TermsNotAgreed":
		return "user hasn't agreed to terms of service"
	case "AccountDeleted":
		return "user account deleted"
	case "AccountDisabled":
		return "user account disabled"
	case "ServiceDisabled":
		return "user access to the service disabled"
	case "ServiceUnavailable":
		return "service unavailable at this time"
	}
	return "unknown error during ClientLogin: " + e.Code
}
