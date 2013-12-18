package rietveld_test

import (
	"code.google.com/p/rsc/codebot/rietveld"
	. "launchpad.net/gocheck"
	"net/http"
	"os"
	"time"
)

type AuthS struct {
	HTTPSuite
	auth        rietveld.Auth
	ui          *dummyUI
	loginURL    string
	rietveldURL string
}

var _ = Suite(&AuthS{})

type dummyUI struct {
	loginURL, prevUser string
}

func (ui *dummyUI) Credentials(loginURL, prevUser string) (user, passwd string, err error) {
	ui.loginURL = loginURL
	ui.prevUser = prevUser
	return "myuser", "mypasswd", nil
}

func (s *AuthS) SetUpTest(c *C) {
	rietveld.SetDebug(true)
	rietveld.SetLogger(c)

	s.ui = &dummyUI{}
	s.loginURL = testServer.URL + "/lurl"
	s.rietveldURL = testServer.URL + "/rurl"
	s.auth = rietveld.NewAuth(s.ui, false, s.loginURL)
}

func (s *AuthS) TestLoginSignLogout(c *C) {
	testServer.Response(200, nil, "Auth=the-auth")

	headers := map[string]string{
		"Set-Cookie": "some=cookie",
		"location":   "http://example.com/marker",
	}
	testServer.Response(302, headers, "")

	err := s.auth.Login(s.rietveldURL, time.Time{})
	c.Assert(err, IsNil)
	c.Assert(s.ui.loginURL, Equals, s.loginURL)
	c.Assert(s.ui.prevUser, Equals, "")

	req := testServer.WaitRequest()
	c.Assert(req.Method, Equals, "POST")
	c.Assert(req.URL.Path, Equals, "/lurl")
	c.Assert(req.Form["Email"], DeepEquals, []string{"myuser"})
	c.Assert(req.Form["Passwd"], DeepEquals, []string{"mypasswd"})
	c.Assert(req.Form["source"], DeepEquals, []string{"goetveld"})
	c.Assert(req.Form["service"], DeepEquals, []string{"ah"})
	c.Assert(req.Form["accountType"], DeepEquals, []string{"GOOGLE"})

	req = testServer.WaitRequest()
	c.Assert(req.Method, Equals, "GET")
	c.Assert(req.URL.Path, Equals, "/rurl/_ah/login")
	c.Assert(req.Form["auth"], DeepEquals, []string{"the-auth"})

	req, err = http.NewRequest("POST", "http://example.com", nil)
	c.Assert(err, IsNil)

	_, err = s.auth.Sign(s.rietveldURL, req)
	c.Assert(err, IsNil)
	c.Assert(req.Header["Cookie"], DeepEquals, []string{"some=cookie"})

	err = s.auth.Logout(s.rietveldURL)
	c.Assert(err, IsNil)

	req, err = http.NewRequest("POST", "http://example.com", nil)
	c.Assert(err, IsNil)

	_, err = s.auth.Sign(s.rietveldURL, req)
	c.Assert(err, IsNil)
	c.Assert(req.Header["Cookie"], IsNil)
}

func (s *AuthS) TestLoginAuthFailed(c *C) {
	testServer.Response(200, nil, "Auth=the-auth")
	testServer.Response(404, nil, "")

	err := s.auth.Login(s.rietveldURL, time.Time{})
	c.Assert(err, ErrorMatches, "error authorizing on rietveld: 404 Not Found")

	testServer.Response(200, nil, "Auth=the-auth")
	headers := map[string]string{"location": "http://example.com"}
	testServer.Response(302, headers, "")

	err = s.auth.Login(s.rietveldURL, time.Time{})
	c.Assert(err, ErrorMatches, "error authorizing on rietveld: .* redirect blocked")
}

func (s *AuthS) TestLoginAfter(c *C) {
	headers := map[string]string{
		"Set-Cookie": "some=cookie",
		"location":   "http://example.com/marker",
	}
	testServer.Response(200, nil, "Auth=the-auth")
	testServer.Response(302, headers, "")

	beforeLogin := time.Now()
	err := s.auth.Login(s.rietveldURL, time.Time{})
	c.Assert(err, IsNil)
	afterLogin := time.Now()

	testServer.WaitRequests(2)

	// This shouldn't attempt any requests.
	err = s.auth.Login(s.rietveldURL, beforeLogin)
	c.Assert(err, IsNil)

	testServer.Response(200, nil, "Auth=the-auth")
	testServer.Response(302, headers, "")

	// But this should attempt to login again.
	err = s.auth.Login(s.rietveldURL, afterLogin)
	c.Assert(err, IsNil)

	testServer.WaitRequests(2)
}

func (s *AuthS) TestSignTiming(c *C) {
	req, err := http.NewRequest("POST", "http://example.com", nil)
	c.Assert(err, IsNil)

	before := time.Now()
	time.Sleep(1e7)
	when, err := s.auth.Sign(s.rietveldURL, req)
	time.Sleep(1e7)
	after := time.Now()

	c.Assert(when.After(before), Equals, true)
	c.Assert(when.Before(after), Equals, true)
}

var errorTests = []struct{ response, msg string }{
	{"", "error reading response: EOF"},
	{"Error=Whatever", "unknown error during ClientLogin: Whatever"},
	{"Error=BadAuthentication", "invalid user or password"},
	{"Error=BadAuthentication\nInfo=InvalidSecondFactor", "application-specific password required"},
	{"Error=CaptchaRequired", "captcha required; visit http://j.mp/unlock-captcha and retry"},
	{"Error=NotVerified", "account not verified"},
	{"Error=TermsNotAgreed", "user hasn't agreed to terms of service"},
	{"Error=AccountDeleted", "user account deleted"},
	{"Error=AccountDisabled", "user account disabled"},
	{"Error=ServiceDisabled", "user access to the service disabled"},
	{"Error=ServiceUnavailable", "service unavailable at this time"},
}

func (s *AuthS) TestLoginError(c *C) {
	for _, t := range errorTests {
		testServer.Response(200, nil, t.response)
		c.Assert(s.auth.Login(s.rietveldURL, time.Time{}), ErrorMatches, t.msg)
	}
}

func (s *AuthS) TestCachedLogin(c *C) {
	defer func(old string) {
		os.Setenv("HOME", old)
	}(os.Getenv("HOME"))
	os.Setenv("HOME", c.MkDir())

	auth := rietveld.NewAuth(s.ui, true, testServer.URL+"/lurl")

	testServer.Response(200, nil, "Auth=the-auth")

	headers := map[string]string{
		"Set-Cookie": "some=cookie",
		"location":   "http://example.com/marker",
	}
	testServer.Response(302, headers, "")

	err := auth.Login(s.rietveldURL, time.Time{})
	c.Assert(err, IsNil)
	c.Assert(s.ui.loginURL, Equals, testServer.URL+"/lurl")
	c.Assert(s.ui.prevUser, Equals, "")

	// Build a new Auth to make use of cached credentials.
	newauth := rietveld.NewAuth(s.ui, true, "")

	req, err := http.NewRequest("POST", "http://example.com", nil)
	c.Assert(err, IsNil)

	_, err = newauth.Sign(s.rietveldURL, req)
	c.Assert(err, IsNil)
	c.Assert(req.Header["Cookie"], DeepEquals, []string{"some=cookie"})

	// Login again to check usage of previousUser on credentials.
	testServer.Response(200, nil, "Auth=the-auth")
	testServer.Response(302, headers, "")
	err = newauth.Login(s.rietveldURL, time.Time{})
	c.Assert(err, IsNil)
	c.Assert(s.ui.loginURL, Equals, s.loginURL)
	c.Assert(s.ui.prevUser, Equals, "myuser")

	// Check that logging out removes cached credentials as well.
	path := os.ExpandEnv("$HOME/.goetveld_localhost:4444")
	stat, err := os.Stat(path)
	c.Assert(stat, NotNil)

	err = newauth.Logout(s.rietveldURL)
	c.Assert(err, IsNil)

	stat, err = os.Stat(path)
	c.Assert(stat, IsNil)

	req, err = http.NewRequest("POST", "http://example.com", nil)
	c.Assert(err, IsNil)

	_, err = newauth.Sign(s.rietveldURL, req)
	c.Assert(err, IsNil)
	c.Assert(req.Header["Cookie"], IsNil)
}
