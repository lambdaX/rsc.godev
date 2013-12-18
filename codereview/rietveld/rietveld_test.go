package rietveld_test

import (
	"bytes"
	"code.google.com/p/rsc/codebot/rietveld"
	"fmt"
	"io"
	"io/ioutil"
	. "launchpad.net/gocheck"
	"mime/multipart"
	"net/http"
	"time"
)

type RietS struct {
	HTTPSuite
	auth *FakeAuth
	riet *rietveld.Rietveld
}

var _ = Suite(&RietS{})

type FakeAuth struct {
	callLog []string
}

var fixedSignTime = time.Unix(123456, 0)

func (fa *FakeAuth) Login(rietveldURL string, after time.Time) error {
	fa.callLog = append(fa.callLog, "Login", rietveldURL)
	if !after.Equal(fixedSignTime) {
		return fmt.Errorf("FakeAuth: want fixedSignTime, got %v", after)
	}
	return nil
}

func (fa *FakeAuth) Logout(rietveldURL string) error {
	fa.callLog = append(fa.callLog, "Logout", rietveldURL)
	return nil
}

func (fa *FakeAuth) Sign(rietveldURL string, req *http.Request) (time.Time, error) {
	fa.callLog = append(fa.callLog, "Sign", rietveldURL)
	req.Header.Set("fake-auth", "true")
	return fixedSignTime, nil
}

type FakeDelta struct {
	baseURL   string
	sendBases bool
}

func (fd *FakeDelta) Patch() ([]*rietveld.FileDiff, error) {
	return []*rietveld.FileDiff{
		&rietveld.FileDiff{rietveld.Modified, "file1", []byte("<diff1>")},
		&rietveld.FileDiff{rietveld.Deleted, "file2", []byte("<diff2>")},
		&rietveld.FileDiff{rietveld.Added, "file3", []byte("<diff3>")},
	}, nil
}

func (fa *FakeDelta) Base(path string) (io.ReadCloser, error) {
	if path == "file1" || path == "file2" {
		return ioutil.NopCloser(bytes.NewBufferString("<base" + path[4:5] + ">")), nil
	}
	return nil, fmt.Errorf("unknown path: %s", path)
}

const baseHashes = "" +
	"6085c2f8ce7b9fcbf42ce2b5473f1945:file1|" +
	"9045b7ae8edfb5199d65151e53994943:file2|" +
	"d41d8cd98f00b204e9800998ecf8427e:file3"

func (fa *FakeDelta) BaseURL() string {
	return fa.baseURL
}

func (fa *FakeDelta) SendBases() bool {
	return fa.sendBases
}

var fakeDeltaResp = `Issue created. URL: http://example.com/123456
42
101 file1
102 file2
103 file3
`

var fakeSimpleResp = "Issue created. URL: http://example.com/123456"

func (s *RietS) SetUpTest(c *C) {
	rietveld.SetDebug(true)
	rietveld.SetLogger(c)
	s.auth = &FakeAuth{}
	s.riet = rietveld.New(testServer.URL, s.auth)
}

func (s *RietS) TestIssueURL(c *C) {
	c.Assert(s.riet.IssueURL(&rietveld.Issue{Id: 123456}), Equals, testServer.URL+"/123456")
}

func (s *RietS) TestSendDelta(c *C) {
	testServer.Responses(1, 200, nil, fakeDeltaResp)
	testServer.Responses(3, 200, nil, "OK")

	issue := &rietveld.Issue{
		Id:            123,
		User:          "user@email.com",
		Subject:       "mysubject",
		Description:   "mydescription",
		ReviewerMails: []string{"r1", "r2"},
		ReviewerNicks: []string{"r3", "r4"},
		CcMails:       []string{"cc1", "cc2"},
		CcNicks:       []string{"cc3", "cc4"},
		BaseURL:       "http://base.url",
		Private:       true,
		Closed:        true,
	}

	delta := &FakeDelta{baseURL: "http://ignore.me", sendBases: true}

	err := s.riet.SendDelta(issue, delta, true)
	c.Assert(err, IsNil)
	c.Assert(issue.Id, Equals, 123456)

	req := testServer.WaitRequest()
	c.Assert(req.Method, Equals, "POST")
	c.Assert(req.URL.Path, Equals, "/upload")
	c.Assert(req.Header.Get("fake-auth"), Equals, "true")
	c.Assert(req.Form["issue"], DeepEquals, []string{"123"})
	c.Assert(req.Form["user"], DeepEquals, []string{"user@email.com"})
	c.Assert(req.Form["subject"], DeepEquals, []string{"mysubject"})
	c.Assert(req.Form["description"], DeepEquals, []string{"mydescription"})
	c.Assert(req.Form["reviewers"], DeepEquals, []string{"r1, r2, r3, r4"})
	c.Assert(req.Form["cc"], DeepEquals, []string{"cc1, cc2, cc3, cc4"})
	c.Assert(req.Form["base"], DeepEquals, []string{"http://base.url"})
	c.Assert(req.Form["private"], DeepEquals, []string{"1"})
	c.Assert(req.Form["closed"], DeepEquals, []string{"1"})
	c.Assert(req.Form["content_upload"], DeepEquals, []string{"1"})
	c.Assert(req.Form["send_mail"], DeepEquals, []string{"1"})
	c.Assert(req.Form["base_hashes"], DeepEquals, []string{baseHashes})

	header := req.MultipartForm.File["data"][0]
	c.Assert(header.Filename, Equals, "data.diff")
	c.Assert(headerData(header), Equals, ""+
		"Index: file1\n<diff1>\n"+
		"Index: file2\n<diff2>\n"+
		"Index: file3\n<diff3>\n")

	req = testServer.WaitRequest()
	c.Assert(req.Method, Equals, "POST")
	c.Assert(req.URL.Path, Equals, "/123456/upload_content/42/101")
	c.Assert(req.Header.Get("fake-auth"), Equals, "true")
	c.Assert(req.Form["status"], DeepEquals, []string{"M"})
	c.Assert(req.Form["filename"], DeepEquals, []string{"file1"})
	c.Assert(req.Form["checksum"], DeepEquals, []string{"6085c2f8ce7b9fcbf42ce2b5473f1945"})
	c.Assert(req.Form["is_binary"], DeepEquals, []string{"false"})
	c.Assert(req.Form["is_current"], DeepEquals, []string{"false"})

	header = req.MultipartForm.File["data"][0]
	c.Assert(header.Filename, Equals, "file1")
	c.Assert(headerData(header), Equals, "<base1>")

	req = testServer.WaitRequest()
	c.Assert(req.Method, Equals, "POST")
	c.Assert(req.URL.Path, Equals, "/123456/upload_content/42/102")
	c.Assert(req.Header.Get("fake-auth"), Equals, "true")
	c.Assert(req.Form["status"], DeepEquals, []string{"D"})
	c.Assert(req.Form["filename"], DeepEquals, []string{"file2"})
	c.Assert(req.Form["checksum"], DeepEquals, []string{"9045b7ae8edfb5199d65151e53994943"})
	c.Assert(req.Form["is_binary"], DeepEquals, []string{"false"})
	c.Assert(req.Form["is_current"], DeepEquals, []string{"false"})

	header = req.MultipartForm.File["data"][0]
	c.Assert(header.Filename, Equals, "file2")
	c.Assert(headerData(header), Equals, "<base2>")

	req = testServer.WaitRequest()
	c.Assert(req.Method, Equals, "POST")
	c.Assert(req.URL.Path, Equals, "/123456/upload_content/42/103")
	c.Assert(req.Header.Get("fake-auth"), Equals, "true")
	c.Assert(req.Form["status"], DeepEquals, []string{"A"})
	c.Assert(req.Form["filename"], DeepEquals, []string{"file3"})
	c.Assert(req.Form["checksum"], DeepEquals, []string{"d41d8cd98f00b204e9800998ecf8427e"})
	c.Assert(req.Form["is_binary"], DeepEquals, []string{"false"})
	c.Assert(req.Form["is_current"], DeepEquals, []string{"false"})

	header = req.MultipartForm.File["data"][0]
	c.Assert(header.Filename, Equals, "file3")
	c.Assert(headerData(header), Equals, "")
}

func (s *RietS) TestMinIssueDetails(c *C) {
	testServer.Response(200, nil, fakeSimpleResp)

	issue := &rietveld.Issue{}

	delta := &FakeDelta{}

	err := s.riet.SendDelta(issue, delta, false)
	c.Assert(err, IsNil)
	c.Assert(issue.Id, Equals, 123456)

	req := testServer.WaitRequest()
	c.Assert(req.Method, Equals, "POST")
	c.Assert(req.URL.Path, Equals, "/upload")
	c.Assert(req.Header.Get("fake-auth"), Equals, "true")
	c.Assert(req.Form["issue"], IsNil)
	c.Assert(req.Form["user"], IsNil)
	c.Assert(req.Form["subject"], DeepEquals, []string{"-"})
	c.Assert(req.Form["description"], IsNil)
	c.Assert(req.Form["reviewers"], IsNil)
	c.Assert(req.Form["cc"], IsNil)
	c.Assert(req.Form["base"], IsNil)
	c.Assert(req.Form["private"], IsNil)
	c.Assert(req.Form["closed"], IsNil)
	c.Assert(req.Form["content_upload"], IsNil)
	c.Assert(req.Form["send_mail"], IsNil)
	c.Assert(req.Form["base_hashes"], DeepEquals, []string{baseHashes})
}

func (s *RietS) TestBaseUploadError(c *C) {
	testServer.Response(200, nil, fakeDeltaResp)

	// The three retries of file1.
	testServer.Response(200, nil, "BOOM1")
	testServer.Response(200, nil, "BOOM2")
	testServer.Response(200, nil, "BOOM3")

	issue := &rietveld.Issue{
		User:        "user@email.com",
		Subject:     "mysubject",
		Description: "mydescription",
	}

	delta := &FakeDelta{baseURL: "http://base.url"}

	err := s.riet.SendDelta(issue, delta, false)
	c.Assert(err, ErrorMatches, "can't upload base of file1: BOOM3")

	req := testServer.WaitRequest()
	c.Assert(req.Form["base"], DeepEquals, []string{"http://base.url"})
}

func (s *RietS) TestBaseURLFromDelta(c *C) {
	testServer.Response(200, nil, fakeSimpleResp)

	issue := &rietveld.Issue{
		User:        "user@email.com",
		Subject:     "mysubject",
		Description: "mydescription",
	}

	delta := &FakeDelta{baseURL: "http://base.url"}

	err := s.riet.SendDelta(issue, delta, false)
	c.Assert(err, IsNil)

	req := testServer.WaitRequest()
	c.Assert(req.Form["base"], DeepEquals, []string{"http://base.url"})
}

func (s *RietS) TestAuth(c *C) {
	testServer.Response(401, nil, "")
	testServer.Response(200, nil, fakeSimpleResp)

	issue := &rietveld.Issue{
		User:        "user@email.com",
		Subject:     "mysubject",
		Description: "mydescription",
	}

	delta := &FakeDelta{baseURL: "http://base.url"}

	err := s.riet.SendDelta(issue, delta, false)
	c.Assert(err, IsNil)

	rurl := testServer.URL
	c.Assert(s.auth.callLog, DeepEquals, []string{"Sign", rurl, "Login", rurl, "Sign", rurl})
}

func (s *RietS) TestAuthWithRedirection(c *C) {
	headers := map[string]string{
		"location": testServer.URL + "/Login",
	}
	testServer.Response(302, headers, "")
	testServer.Response(200, nil, fakeSimpleResp)

	issue := &rietveld.Issue{
		User:        "user@email.com",
		Subject:     "mysubject",
		Description: "mydescription",
	}

	delta := &FakeDelta{baseURL: "http://base.url"}

	err := s.riet.SendDelta(issue, delta, false)
	c.Assert(err, IsNil)

	rurl := testServer.URL
	c.Assert(s.auth.callLog, DeepEquals, []string{"Sign", rurl, "Login", rurl, "Sign", rurl})
}

func headerData(h *multipart.FileHeader) string {
	file, err := h.Open()
	if err != nil {
		return "ERROR: " + err.Error()
	}
	data, err := ioutil.ReadAll(file)
	file.Close()
	if err != nil {
		return "ERROR: " + err.Error()
	}
	return string(data)
}

func (s *RietS) TestLoadIssueBadData1(c *C) {
	testServer.ResponseMap(4, ResponseMap{
		"/api/5372097":     Response{Body: testData("issue.json")},
		"/5372097/publish": Response{Body: "<html></html>"},
	})
	_, err := s.riet.Issue(5372097)
	c.Assert(err, ErrorMatches, "can't parse /publish form")
	testServer.WaitRequests(4)
}

func (s *RietS) TestLoadIssueBadData2(c *C) {
	testServer.ResponseMap(4, ResponseMap{
		"/api/5372097":     Response{Body: "}{"},
		"/5372097/publish": Response{Body: testData("publish.html")},
	})
	_, err := s.riet.Issue(5372097)
	c.Assert(err, ErrorMatches, "can't unmarshal issue JSON: invalid.*")
	testServer.WaitRequests(4)
}

//func (s *RietS) TestLoadIssueBadStatus(c *C) {
//	testServer.Response(404, nil, "<html></html>")
//	testServer.Response(404, nil, "<html></html>")
//	testServer.Response(404, nil, "<html></html>")
//
//	_, err := s.riet.Issue(5372097)
//	c.Assert(err, ErrorMatches, `server returned "404 Not Found"`)
//
//	for i := 0; i != 3; i++ {
//		req := testServer.WaitRequest()
//		c.Assert(req.Method, Equals, "GET")
//		c.Assert(req.URL.Path, Equals, "/5372097/edit")
//	}
//}

func testData(filename string) string {
	data, err := ioutil.ReadFile("testdata/" + filename)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func (s *RietS) TestLoadIssue(c *C) {
	// Requests are concurrent.
	testServer.ResponseMap(2, ResponseMap{
		"/api/5372097":     Response{Body: testData("issue.json")},
		"/5372097/publish": Response{Body: testData("publish.html")},
	})

	issue, err := s.riet.Issue(5372097)
	c.Assert(err, IsNil)
	c.Assert(issue.Id, Equals, 5372097)
	c.Assert(issue.Subject, Equals, "Test subject")
	c.Assert(issue.Description, Equals, "Test description.")
	c.Assert(issue.ReviewerMails, DeepEquals, []string{"r1@e.c", "r2@e.c"})
	c.Assert(issue.ReviewerNicks, DeepEquals, []string{"r1", "r2"})
	c.Assert(issue.CcMails, DeepEquals, []string{"cc1@e.c", "cc2@e.c"})
	c.Assert(issue.CcNicks, DeepEquals, []string{"cc1", "cc2"})
	c.Assert(issue.Private, Equals, true)
	c.Assert(issue.Closed, Equals, true)
}

//func (s *RietS) TestUpdateIssueBadStatus(c *C) {
//	html, err := ioutil.ReadFile("testdata/edit.html")
//	c.Assert(err, IsNil)
//
//	testServer.Response(200, nil, string(html))
//
//	issue, err := s.riet.Issue(5372097)
//	c.Assert(err, IsNil)
//	testServer.Response(404, nil, "<html></html>")
//	testServer.Response(404, nil, "<html></html>")
//	testServer.Response(404, nil, "<html></html>")
//
//	err = s.riet.UpdateIssue(issue)
//	c.Assert(err, ErrorMatches, `server returned "404 Not Found"`)
//
//	req := testServer.WaitRequest()
//	c.Assert(req.Method, Equals, "GET")
//	c.Assert(req.URL.Path, Equals, "/5372097/edit")
//
//	for i := 0; i != 3; i++ {
//		req := testServer.WaitRequest()
//		c.Assert(req.Method, Equals, "POST")
//		c.Assert(req.URL.Path, Equals, "/5372097/edit")
//	}
//}

func (s *RietS) TestUpdateIssue1(c *C) {
	html, err := ioutil.ReadFile("testdata/edit.html")
	c.Assert(err, IsNil)

	testServer.Response(200, nil, string(html))
	testServer.Response(200, nil, "")

	issue := &rietveld.Issue{
		Id:            5372097,
		Subject:       "Test subject",
		Description:   "Test description.",
		ReviewerMails: []string{"r1@e.c", "r2@e.c"},
		ReviewerNicks: []string{"r3", "r4"},
		CcMails:       []string{"cc1@e.c", "cc2@e.c"},
		CcNicks:       []string{"cc3", "cc4"},
		Private:       true,
		Closed:        true,
	}

	err = s.riet.UpdateIssue(issue)
	c.Assert(err, IsNil)

	get := testServer.WaitRequest()
	post := testServer.WaitRequest()
	if get.Method == "POST" {
		get, post = post, get
	}

	c.Assert(get.Method, Equals, "GET")
	c.Assert(get.URL.Path, Equals, "/5372097/edit")

	c.Assert(post.Method, Equals, "POST")
	c.Assert(post.URL.Path, Equals, "/5372097/edit")
	c.Assert(post.Form["xsrf_token"], DeepEquals, []string{"515c6d74d6c8ffd1d4a1cb980e54ff84"})
	c.Assert(post.Form["subject"], DeepEquals, []string{"Test subject"})
	c.Assert(post.Form["description"], DeepEquals, []string{"Test description."})
	c.Assert(post.Form["reviewers"], DeepEquals, []string{"r1@e.c, r2@e.c, r3, r4"})
	c.Assert(post.Form["cc"], DeepEquals, []string{"cc1@e.c, cc2@e.c, cc3, cc4"})
	c.Assert(post.Form["private"], DeepEquals, []string{"checked"})
	c.Assert(post.Form["closed"], DeepEquals, []string{"checked"})
}

func (s *RietS) TestUpdateIssue2(c *C) {
	html, err := ioutil.ReadFile("testdata/edit.html")
	c.Assert(err, IsNil)

	// Also check that it accepts redirections fine.
	headers := map[string]string{
		"location": testServer.URL + "/dont-load",
	}

	testServer.Response(200, nil, string(html))
	testServer.Response(302, headers, "")

	issue := &rietveld.Issue{
		Id:          5372097,
		Subject:     "Test subject",
		Description: "Test description.",
		Private:     false,
		Closed:      false,
	}

	err = s.riet.UpdateIssue(issue)
	c.Assert(err, IsNil)

	get := testServer.WaitRequest()
	post := testServer.WaitRequest()
	if get.Method == "POST" {
		get, post = post, get
	}

	c.Assert(get.Method, Equals, "GET")
	c.Assert(get.URL.Path, Equals, "/5372097/edit")

	c.Assert(post.Method, Equals, "POST")
	c.Assert(post.URL.Path, Equals, "/5372097/edit")
	c.Assert(post.Form["xsrf_token"], DeepEquals, []string{"515c6d74d6c8ffd1d4a1cb980e54ff84"})
	c.Assert(post.Form["subject"], DeepEquals, []string{"Test subject"})
	c.Assert(post.Form["description"], DeepEquals, []string{"Test description."})
	c.Assert(post.Form["reviewers"], DeepEquals, []string{""})
	c.Assert(post.Form["cc"], DeepEquals, []string{""})
	c.Assert(post.Form["private"], DeepEquals, []string{""})
	c.Assert(post.Form["closed"], DeepEquals, []string{""})
}

func (s *RietS) TestUpdateIssueReviewers(c *C) {

	for testn := 0; testn < 2; testn++ {

		// Requests are concurrent.
		testServer.ResponseMap(2, ResponseMap{
			"/api/5372097":     Response{Body: testData("issue.json")},
			"/5372097/publish": Response{Body: testData("publish.html")},
		})

		html, err := ioutil.ReadFile("testdata/edit.html")
		c.Assert(err, IsNil)

		issue, err := s.riet.Issue(5372097)
		c.Assert(err, IsNil)

		testServer.WaitRequest()
		testServer.WaitRequest()

		testServer.Response(200, nil, string(html))
		testServer.Response(200, nil, "")

		if testn == 0 {
			issue.ReviewerMails = []string{"r5"}
			issue.CcMails = []string{"c5"}
		} else {
			issue.ReviewerNicks = []string{"r5"}
			issue.CcNicks = []string{"c5"}
		}

		err = s.riet.UpdateIssue(issue)
		c.Assert(err, IsNil)

		get := testServer.WaitRequest()
		post := testServer.WaitRequest()
		if get.Method == "POST" {
			get, post = post, get
		}

		c.Assert(get.Method, Equals, "GET")
		c.Assert(get.URL.Path, Equals, "/5372097/edit")

		c.Assert(post.Method, Equals, "POST")
		c.Assert(post.URL.Path, Equals, "/5372097/edit")
		c.Assert(post.Form["xsrf_token"], DeepEquals, []string{"515c6d74d6c8ffd1d4a1cb980e54ff84"})
		c.Assert(post.Form["subject"], DeepEquals, []string{"Test subject"})
		c.Assert(post.Form["description"], DeepEquals, []string{"Test description."})
		c.Assert(post.Form["reviewers"], DeepEquals, []string{"r5"})
		c.Assert(post.Form["cc"], DeepEquals, []string{"c5"})
		c.Assert(post.Form["private"], DeepEquals, []string{"checked"})
		c.Assert(post.Form["closed"], DeepEquals, []string{"checked"})
	}
}

func (s *RietS) TestAddComment1(c *C) {
	html, err := ioutil.ReadFile("testdata/publish.html")
	c.Assert(err, IsNil)

	testServer.Response(200, nil, string(html))
	testServer.Response(200, nil, "")

	issue := &rietveld.Issue{Id: 5418043}
	comment := &rietveld.Comment{Message: "Test message.", NoMail: true}

	err = s.riet.AddComment(issue, comment)
	c.Assert(err, IsNil)

	req := testServer.WaitRequest()
	c.Assert(req.Method, Equals, "GET")
	c.Assert(req.URL.Path, Equals, "/5418043/publish")

	req = testServer.WaitRequest()
	c.Assert(req.Method, Equals, "POST")
	c.Assert(req.URL.Path, Equals, "/5418043/publish")
	c.Assert(req.Form["xsrf_token"], DeepEquals, []string{"aadc0b2909b997436e62dea10a3ccb13"})
	c.Assert(req.Form["message"], DeepEquals, []string{"Test message."})
	c.Assert(req.Form["reviewers"], DeepEquals, []string{"r1, r2"})
	c.Assert(req.Form["cc"], DeepEquals, []string{"cc1, cc2"})
	c.Assert(req.Form["message_only"], DeepEquals, []string{"true"})
	c.Assert(req.Form["send_mail"], DeepEquals, []string{""})
	c.Assert(req.Form["no_redirect"], DeepEquals, []string{"true"})
}

func (s *RietS) TestAddComment2(c *C) {
	html, err := ioutil.ReadFile("testdata/publish_owner.html")
	c.Assert(err, IsNil)

	testServer.Response(200, nil, string(html))
	testServer.Response(200, nil, "")

	issue := &rietveld.Issue{Id: 5418043}
	comment := &rietveld.Comment{
		Subject:   "Test subject",
		Message:   "Test message.",
		Reviewers: []string{"r3", "r4"},
		Cc:        []string{"cc3", "cc4"},
	}

	err = s.riet.AddComment(issue, comment)
	c.Assert(err, IsNil)

	req := testServer.WaitRequest()
	c.Assert(req.Method, Equals, "GET")
	c.Assert(req.URL.Path, Equals, "/5418043/publish")

	req = testServer.WaitRequest()
	c.Assert(req.Method, Equals, "POST")
	c.Assert(req.URL.Path, Equals, "/5418043/publish")
	c.Assert(req.Form["xsrf_token"], DeepEquals, []string{"aadc0b2909b997436e62dea10a3ccb13"})
	c.Assert(req.Form["subject"], DeepEquals, []string{"Test subject"})
	c.Assert(req.Form["message"], DeepEquals, []string{"Test message."})
	c.Assert(req.Form["reviewers"], DeepEquals, []string{"r3, r4"})
	c.Assert(req.Form["cc"], DeepEquals, []string{"cc3, cc4"})
	c.Assert(req.Form["message_only"], DeepEquals, []string{""})
	c.Assert(req.Form["send_mail"], DeepEquals, []string{"checked"})
	c.Assert(req.Form["no_redirect"], DeepEquals, []string{"true"})
}

func (s *RietS) TestAddComment3(c *C) {
	html, err := ioutil.ReadFile("testdata/publish_owner.html")
	c.Assert(err, IsNil)

	testServer.Response(200, nil, string(html))
	testServer.Response(200, nil, "")

	issue := &rietveld.Issue{Id: 5418043}
	comment := &rietveld.Comment{
		Subject:       "Test subject",
		Message:       "Test message.",
		PublishDrafts: true,
	}

	err = s.riet.AddComment(issue, comment)
	c.Assert(err, IsNil)

	req := testServer.WaitRequest()
	c.Assert(req.Method, Equals, "GET")
	c.Assert(req.URL.Path, Equals, "/5418043/publish")

	req = testServer.WaitRequest()
	c.Assert(req.Method, Equals, "POST")
	c.Assert(req.URL.Path, Equals, "/5418043/publish")
	c.Assert(req.Form["xsrf_token"], DeepEquals, []string{"aadc0b2909b997436e62dea10a3ccb13"})
	c.Assert(req.Form["subject"], DeepEquals, []string{"Test subject"})
	c.Assert(req.Form["message"], DeepEquals, []string{"Test message."})
	c.Assert(req.Form["reviewers"], DeepEquals, []string{"r1, r2"})
	c.Assert(req.Form["cc"], DeepEquals, []string{"cc1, cc2"})
	c.Assert(req.Form["message_only"], DeepEquals, []string{""})
	c.Assert(req.Form["send_mail"], DeepEquals, []string{"checked"})
	c.Assert(req.Form["no_redirect"], DeepEquals, []string{"true"})
}
