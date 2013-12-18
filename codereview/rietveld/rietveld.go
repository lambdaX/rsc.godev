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
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"
)

// The Rietveld type encapsulates the communication with a rietveld server.
type Rietveld struct {
	url    string
	auth   Auth
	client *http.Client
}

// New returns a new *Rietveld capable of communicating with the
// server at rietveldURL, and authenticating requests using auth.
func New(rietveldURL string, auth Auth, t http.RoundTripper) *Rietveld {
	return &Rietveld{rietveldURL, auth, &http.Client{Transport: t}}
}

// CodeReview is a *Rietveld that can communicate with the standard
// deployment of Rietveld on Google App Engine, using DefaultAuth
// for credentials management.
var CodeReview = New("https://codereview.appspot.com", DefaultAuth, http.DefaultTransport)

// The Issue type represents a code review entry in Rietveld.
type Issue struct {
	Id          int
	User        string
	Subject     string
	Description string
	BaseURL     string
	Private     bool
	Closed      bool

	// When an issue is loaded, ReviewerMails and ReviewerNicks
	// represent the same list of addresses in different formats.
	// When an issue is changed or created, either list may be used
	// with either format and the server will recognize them correctly.
	ReviewerMails []string
	ReviewerNicks []string

	// Ditto.
	CcMails []string
	CcNicks []string

	// Given the duplication between nicks and non-nicks, must
	// be able to tell if any list has changed so that an update
	// is properly reflected.
	origReviewerMails []string
	origReviewerNicks []string
	origCcMails       []string
	origCcNicks       []string
}

// Comment holds a message to be added to an issue's thread.
type Comment struct {
	Subject string
	Message string

	// If Reviewers and/or Cc is not nil, the respective list of people
	// in the issue will be updated to the provided list before the
	// comment is added.
	Reviewers []string
	Cc        []string

	// If NoMail is true, do not mail people when adding comment.
	NoMail bool

	// If PublishDrafts is true, inline comments made in the code
	// and not yet published will be delivered.
	PublishDrafts bool
}

// IssueURL returns the URL for the given issue.
func (r *Rietveld) IssueURL(issue *Issue) string {
	return fmt.Sprintf("%s/%d", r.url, issue.Id)
}

type prepareFunc func(op *opInfo) (*http.Request, error)

type opInfo struct {
	r       *Rietveld
	issue   *Issue
	delta   Delta
	patch   []*FileDiff
	baseMD5 map[string]string

	psId     string
	psPathId map[string]string
	psNoBase map[string]bool
}

// SendDelta sends the code change specified in delta to the given issue.
// If the issue Id is zero, a new issue will be created with the remaining
// issue details and the new issue id will be assigned to the issue field Id.
// If sendMail is true, the review request will be mailed as soon as it is
// created.
func (r *Rietveld) SendDelta(issue *Issue, delta Delta, sendMail bool) error {
	patch, err := delta.Patch()
	if err != nil {
		return err
	}

	op := &opInfo{r: r, issue: issue, delta: delta, patch: patch}

	if err := r.do(&uploadHandler{op, sendMail}); err != nil {
		return err
	}

	for _, diff := range op.patch {
		path := diff.Path
		if op.psPathId[path] == "" {
			logf("Base for %s not requested.", path)
			continue
		}
		if op.psNoBase[path] {
			logf("Base for %s already on server.", path)
			continue
		}

		if err := r.do(&baseUploadHandler{op, path}); err != nil {
			return err
		}
	}

	return nil
}

func dumpResponse(resp *http.Response) {
	dump, err := httputil.DumpResponse(resp, true)
	if err != nil {
		println("Error dumping response: " + err.Error())
		return
	}
	println("----------------------------------------------------------")
	println(string(dump))
	println("----------------------------------------------------------")
}

const maxRetries = 3

func (r *Rietveld) do(handler requestHandler) (err error) {
	// NOTE: err variables in this function must not be shadowed so that
	//       if maxRetries is exhausted the error is meaningful.
	var req *http.Request
	var resp *http.Response
	var signTime time.Time
	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			logf("Retrying...")
		}
		pr, pw := io.Pipe()
		mpw := multipart.NewWriter(pw)
		method, path := handler.action()
		req, err = http.NewRequest(method, r.url+path, pr)
		if err != nil {
			return err
		}

		signTime, err = r.auth.Sign(r.url, req)
		if err != nil {
			return err
		}

		req.Header.Set("Content-Type", mpw.FormDataContentType())
		go func() {
			if err := handler.write(mpw); err != nil {
				logf("Failed to prepare request: %v", err)
				pw.CloseWithError(err)
				return
			}
			mpw.Close()
			pw.Close()
		}()

		resp, err = r.client.Do(req)
		req.Body.Close()
		if err != nil {
			logf("Request failed: %v", err)
			continue
		}
		sc := resp.StatusCode
		if sc == 401 || sc == 302 && strings.Index(resp.Header.Get("location"), "Login") > 0 {
			resp.Body.Close()
			if i+1 == maxRetries {
				return fmt.Errorf("server returned %q", resp.Status)
			}
			logf("Server returned %q. Retrying after login...", resp.Status)
			err = r.auth.Login(r.url, signTime, r.client.Transport)
			if err != nil {
				return err
			}
			continue
		}
		err = handler.process(resp)
		resp.Body.Close()
		if err != nil {
			logf("Failed to process response: %v", err)
			continue
		}
		break
	}
	return err
}

type requestHandler interface {
	action() (method, path string)
	write(mpw *multipart.Writer) error
	process(resp *http.Response) error
}

type uploadHandler struct {
	op       *opInfo
	sendMail bool
}

func (h *uploadHandler) action() (method, path string) {
	return "POST", "/upload"
}

func (h *uploadHandler) write(mpw *multipart.Writer) error {
	op := h.op
	issue := op.issue
	if issue.Id == 0 {
		logf("Uploading delta to new issue...")
	} else {
		logf("Uploading delta to issue %d...", issue.Id)
	}

	hashes, err := h.baseHashes()
	if err != nil {
		return fmt.Errorf("computing base hashes: %v", err)
	}

	fields := map[string]string{"base_hashes": hashes}

	//"send_mail": "1",
	//"separate_patches": "1",

	if issue.Id > 0 {
		fields["issue"] = strconv.Itoa(issue.Id)
	}
	if issue.Subject == "" {
		fields["subject"] = "-"
	} else {
		fields["subject"] = issue.Subject
	}
	if issue.Description != "" {
		fields["description"] = issue.Description
	}
	if issue.User != "" {
		fields["user"] = issue.User
	}
	var reviewers, cc []string
	if len(issue.ReviewerMails) > 0 {
		reviewers = append(reviewers, issue.ReviewerMails...)
	}
	if len(issue.ReviewerNicks) > 0 {
		reviewers = append(reviewers, issue.ReviewerNicks...)
	}
	if len(reviewers) > 0 {
		fields["reviewers"] = strings.Join(reviewers, ", ")
	}
	if len(issue.CcMails) > 0 {
		cc = append(cc, issue.CcMails...)
	}
	if len(issue.CcNicks) > 0 {
		cc = append(cc, issue.CcNicks...)
	}
	if len(cc) > 0 {
		fields["cc"] = strings.Join(cc, ", ")
	}
	if issue.Private {
		fields["private"] = "1"
	}
	if issue.Closed {
		fields["closed"] = "1"
	}
	baseURL := issue.BaseURL
	if baseURL == "" {
		baseURL = op.delta.BaseURL()
	}
	if baseURL != "" {
		fields["base"] = baseURL
	}
	if op.delta.SendBases() {
		fields["content_upload"] = "1"
	}
	if h.sendMail {
		fields["send_mail"] = "1"
	}

	if err := writeFields(mpw, fields); err != nil {
		return err
	}

	data, err := mpw.CreateFormFile("data", "data.diff")
	if err != nil {
		return err
	}

	for _, diff := range op.patch {
		_, err = data.Write([]byte("Index: " + diff.Path + "\n"))
		if err != nil {
			return err
		}
		// XXX Skip original Index: line from text.
		_, err = data.Write(diff.Text)
		if err != nil {
			return err
		}
		_, err = data.Write([]byte{'\n'})
		if err != nil {
			return err
		}
	}
	return nil
}

func (h *uploadHandler) baseHashes() (hashes string, err error) {
	op := h.op
	op.baseMD5 = make(map[string]string, len(op.patch))
	hash := md5.New()
	buf := make([]byte, 0, hash.Size()*4*len(op.patch))
	hexbuf := make([]byte, hash.Size()*2)
	for i, diff := range op.patch {
		if i > 0 {
			buf = append(buf, '|')
		}
		if diff.Op == Added {
			copy(hexbuf, "d41d8cd98f00b204e9800998ecf8427e")
		} else {
			base, err := op.delta.Base(diff.Path)
			if err != nil {
				return "", err
			}
			hash.Reset()
			_, err = io.Copy(hash, base)
			base.Close()
			if err != nil {
				return "", err
			}
			hex.Encode(hexbuf, hash.Sum(nil))
		}
		buf = append(buf, hexbuf...)
		buf = append(buf, ':')
		buf = append(buf, []byte(diff.Path)...)
		op.baseMD5[diff.Path] = string(hexbuf)
	}
	return string(buf), nil
}

func (h *uploadHandler) process(resp *http.Response) error {
	buf := bufio.NewReader(resp.Body)
	status, err := readLine(buf, true)
	if err != nil {
		return err
	}
	logf("Response from server: %s", status)

	op := h.op
	if strings.HasPrefix(status, "Issue created.") {
		i := strings.LastIndex(status, "/")
		if i < 0 {
			return fmt.Errorf("can't find issue id in response: %s", status)
		}
		op.issue.Id, err = strconv.Atoi(status[i+1:])
		if err != nil {
			return fmt.Errorf("can't parse issue id in response: %s", status)
		}
	} else if !strings.HasPrefix(status, "Issue updated.") {
		return errors.New(status)
	}

	op.psId, err = readLine(buf, false)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}

	op.psPathId = make(map[string]string, len(op.patch))
	op.psNoBase = make(map[string]bool)

	for {
		line, err := readLine(buf, false)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		noBase := strings.HasPrefix(line, "nobase_")
		if noBase {
			line = line[7:]
		}

		fields := strings.SplitN(line, " ", 2)
		if len(fields) != 2 {
			logf("Warning: bad patchset file id line: %s", line)
		}

		op.psPathId[fields[1]] = fields[0]
		if noBase {
			op.psNoBase[fields[1]] = true
		}
	}
	panic("unreachable")
}

type baseUploadHandler struct {
	op       *opInfo
	filepath string
}

func (h *baseUploadHandler) action() (method, path string) {
	return "POST", fmt.Sprintf("/%d/upload_content/%s/%s", h.op.issue.Id, h.op.psId, h.op.psPathId[h.filepath])
}

func (h *baseUploadHandler) write(mpw *multipart.Writer) error {
	logf("Uploading base of %s...", h.filepath)

	var diff *FileDiff
	for _, d := range h.op.patch {
		if d.Path == h.filepath {
			diff = d
			break
		}
	}
	if diff == nil {
		return fmt.Errorf("file %s is not part of patch", h.filepath)
	}

	fields := map[string]string{
		"filename":   h.filepath,
		"status":     string(diff.Op),
		"is_binary":  "false",
		"is_current": "false",
		"checksum":   h.op.baseMD5[h.filepath],
	}

	//"file_too_large": "1",

	if err := writeFields(mpw, fields); err != nil {
		return err
	}

	data, err := mpw.CreateFormFile("data", h.filepath)
	if err != nil {
		return err
	}

	if diff.Op == Added {
		return nil
	}

	base, err := h.op.delta.Base(h.filepath)
	if err != nil {
		return err
	}
	_, err = io.Copy(data, base)
	base.Close()
	return err
}

func (h *baseUploadHandler) process(resp *http.Response) error {
	buf := bufio.NewReader(resp.Body)
	status, err := readLine(buf, true)
	if err != nil {
		return err
	}
	logf("Response from server: %s", status)
	if status != "OK" {
		return fmt.Errorf("can't upload base of %s: %s", h.filepath, status)
	}
	return nil
}

func readLine(buf *bufio.Reader, required bool) (string, error) {
	line, prefix, err := buf.ReadLine()
	if err != nil {
		if required || err != io.EOF {
			err = fmt.Errorf("error reading response: %v", err)
		}
		return "", err
	}
	if prefix {
		return "", errors.New("server response line is too long")
	}
	return string(line), nil
}

func writeFields(mpw *multipart.Writer, fields map[string]string) error {
	for k, v := range fields {
		err := mpw.WriteField(k, v)
		if err != nil {
			return err
		}
	}
	return nil
}
