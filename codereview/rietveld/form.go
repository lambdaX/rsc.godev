package rietveld

import (
	"bytes"
	"code.google.com/p/rsc/codebot/rietveld/html"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"reflect"
	"strings"
)

// Issue retrieves the existing issue with the provided id from Rietveld.
func (r *Rietveld) Issue(id int) (*Issue, error) {
	issue := &Issue{Id: id}
	op := &opInfo{r: r, issue: issue}
	errs := make(chan error, 2)
	iload := &issueLoadHandler{op}
	pload := &publishLoadHandler{op: op, updateIssue: true}
	go func() { errs <- r.do(iload) }()
	go func() { errs <- r.do(pload) }()
	err := firstError(2, errs)
	if err != nil {
		return nil, err
	}
	return issue, nil
}

// UpdateIssue changes the server representation of the provided
// issue to match all of its field values.
// The issue must necessarily have been loaded with the Issue method
func (r *Rietveld) UpdateIssue(issue *Issue) error {
	op := &opInfo{r: r, issue: issue}
	// Two requests concurrently, even though the second depends on
	// the result of the first. How about that?
	errs := make(chan error)
	ch := make(chan map[string]string, 1)
	go func() {
		errs <- r.do(&editLoadHandler{op: op, form: ch})
		close(ch)
	}()
	go func() {
		errs <- r.do(&editHandler{op: op, form: ch})
	}()
	return firstError(2, errs)
}

func firstError(n int, errors chan error) error {
	for i := 0; i < n; i++ {
		if err := <-errors; err != nil {
			return err
		}
	}
	return nil
}

// AddComment appends comment to the conversation thread of issue,
// and update it according to the provided settings.
func (r *Rietveld) AddComment(issue *Issue, comment *Comment) error {
	op := &opInfo{r: r, issue: issue}
	load := &publishLoadHandler{op: op}
	if err := r.do(load); err != nil {
		return err
	}
	publish := &publishHandler{op, load.form, comment}
	return r.do(publish)
}

type issueLoadHandler struct {
	op *opInfo
}

func (h *issueLoadHandler) action() (method, path string) {
	return "GET", fmt.Sprintf("/api/%d", h.op.issue.Id)
}

func (h *issueLoadHandler) write(mpw *multipart.Writer) error {
	logf("Requesting details for issue %d...", h.op.issue.Id)
	return nil
}

func (h *issueLoadHandler) process(resp *http.Response) error {
	debugf("Response from server: %s", resp.Status)
	if resp.StatusCode != 200 {
		return fmt.Errorf("server returned %q", resp.Status)
	}
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("can't read server response: %v", err)
	}

	fields := make(map[string]interface{})
	err = json.Unmarshal(data, &fields)
	if err != nil {
		return fmt.Errorf("can't unmarshal issue JSON: %v", err)
	}

	issue := h.op.issue
	issue.Subject = jsonString(fields["subject"])
	issue.Description = jsonString(fields["description"])
	issue.ReviewerMails = jsonStringSlice(fields["reviewers"])
	issue.CcMails = jsonStringSlice(fields["cc"])
	issue.origReviewerMails = append([]string(nil), issue.ReviewerMails...)
	issue.origCcMails = append([]string(nil), issue.CcMails...)
	issue.Private = jsonBool(fields["private"])
	issue.Closed = jsonBool(fields["closed"])
	return nil
}

type editLoadHandler struct {
	op   *opInfo
	form chan map[string]string
}

func (h *editLoadHandler) action() (method, path string) {
	return "GET", fmt.Sprintf("/%d/edit", h.op.issue.Id)
}

func (h *editLoadHandler) write(mpw *multipart.Writer) error {
	logf("Requesting details for issue %d...", h.op.issue.Id)
	return nil
}

func (h *editLoadHandler) process(resp *http.Response) error {
	debugf("Response from server: %s", resp.Status)
	if resp.StatusCode != 200 {
		return fmt.Errorf("server returned %q", resp.Status)
	}
	form, err := parseForm("/edit", resp.Body)
	if err == nil {
		h.form <- form
	}
	return err
}

type editHandler struct {
	op   *opInfo
	form <-chan map[string]string
}

func (h *editHandler) action() (method, path string) {
	return "POST", fmt.Sprintf("/%d/edit", h.op.issue.Id)
}

// newAddresses returns the new list of addresses considering what
// was changed since the issue was originally loaded. This is
// necessary because the Mails and Nicks lists contain the same
// addresses in different formats, and we want the intuitive
// changing of a single one of these lists to work as expected.
func newAddresses(oldMails, newMails, oldNicks, newNicks []string) (addrs []string) {
	diffMails := !reflect.DeepEqual(oldMails, newMails)
	diffNicks := !reflect.DeepEqual(oldNicks, newNicks)
	if diffMails && diffNicks {
		// Send both. The server will deduplicate.
		addrs = make([]string, 0, len(newMails)+len(newNicks))
		addrs = append(addrs, newMails...)
		addrs = append(addrs, newNicks...)
	} else if diffMails {
		addrs = newMails
	} else {
		addrs = newNicks
	}
	return
}

func (h *editHandler) write(mpw *multipart.Writer) error {
	logf("Updating details of issue %d...", h.op.issue.Id)
	issue := h.op.issue
	form, ok := <-h.form
	if !ok {
		return fmt.Errorf("updating of issue was aborted")
	}

	rv := newAddresses(issue.origReviewerMails, issue.ReviewerMails, issue.origReviewerNicks, issue.ReviewerNicks)
	cc := newAddresses(issue.origCcMails, issue.CcMails, issue.origCcNicks, issue.CcNicks)

	form["subject"] = issue.Subject
	form["description"] = issue.Description
	form["reviewers"] = strings.Join(rv, ", ")
	form["cc"] = strings.Join(cc, ", ")
	form["private"] = checked(issue.Private)
	form["closed"] = checked(issue.Closed)
	return writeFields(mpw, form)
}

func (h *editHandler) process(resp *http.Response) error {
	debugf("Response from server: %s", resp.Status)
	if resp.StatusCode != 200 && resp.StatusCode != 302 {
		return fmt.Errorf("server returned %q", resp.Status)
	}
	return nil
}

func checked(ticked bool) string {
	if ticked {
		return "checked"
	}
	return ""
}

type publishLoadHandler struct {
	op          *opInfo
	form        map[string]string
	updateIssue bool
}

func (h *publishLoadHandler) action() (method, path string) {
	return "GET", fmt.Sprintf("/%d/publish", h.op.issue.Id)
}

func (h *publishLoadHandler) write(mpw *multipart.Writer) error {
	logf("Requesting commenting details for issue %d...", h.op.issue.Id)
	return nil
}

func (h *publishLoadHandler) process(resp *http.Response) error {
	debugf("Response from server: %s", resp.Status)
	if resp.StatusCode != 200 {
		return fmt.Errorf("server returned %q", resp.Status)
	}
	form, err := parseForm("/publish", resp.Body)
	if err != nil {
		return err
	}
	h.form = form
	if h.updateIssue {
		issue := h.op.issue
		issue.ReviewerNicks = strings.Split(form["reviewers"], ", ")
		issue.CcNicks = strings.Split(form["cc"], ", ")
		issue.origReviewerNicks = append([]string(nil), issue.ReviewerNicks...)
		issue.origCcNicks = append([]string(nil), issue.CcNicks...)
	}
	return nil
}

type publishHandler struct {
	op      *opInfo
	form    map[string]string
	comment *Comment
}

func (h *publishHandler) action() (method, path string) {
	return "POST", fmt.Sprintf("/%d/publish", h.op.issue.Id)
}

func (h *publishHandler) write(mpw *multipart.Writer) error {
	logf("Adding comment to issue %d...", h.op.issue.Id)
	form := h.form
	c := h.comment
	if _, ok := form["subject"]; ok {
		if c.Subject != "" {
			form["subject"] = c.Subject
		}
	} else if c.Subject != "" {
		return fmt.Errorf("can't change subject of issue %d (not owner?)", h.op.issue.Id)
	}
	form["message"] = c.Message
	if c.PublishDrafts {
		form["message_only"] = ""
	} else {
		form["message_only"] = "true"
	}
	if c.Reviewers != nil {
		form["reviewers"] = strings.Join(c.Reviewers, ", ")
		form["message_only"] = ""
	}
	if c.Cc != nil {
		form["cc"] = strings.Join(c.Cc, ", ")
		form["message_only"] = ""
	}
	form["send_mail"] = checked(!c.NoMail)
	form["no_redirect"] = "true"
	return writeFields(mpw, form)
}

func (h *publishHandler) process(resp *http.Response) error {
	debugf("Response from server: %s", resp.Status)
	if resp.StatusCode != 200 {
		return fmt.Errorf("server returned %q", resp.Status)
	}
	if h.form != nil {
		return nil
	}
	form, err := parseForm("/publish", resp.Body)
	if err != nil {
		return err
	}
	h.form = form
	return nil
}

var (
	formBytes     = []byte("form")
	actionBytes   = []byte("action")
	nameBytes     = []byte("name")
	valueBytes    = []byte("value")
	inputBytes    = []byte("input")
	checkedBytes  = []byte("checked")
	textareaBytes = []byte("textarea")
)

func parseForm(actionSuffix string, r io.Reader) (form map[string]string, err error) {
	form = make(map[string]string)
	z := html.NewTokenizer(r)
	inForm := false
	inTextArea := ""
	actionSuffixBytes := []byte(actionSuffix)
loop:
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			err := z.Err()
			if err == io.EOF {
				break loop
			}
			return nil, err
		case html.StartTagToken, html.SelfClosingTagToken:
			var key, val []byte
			tag, attr := z.TagName()
			if bytes.Equal(tag, formBytes) && tt == html.StartTagToken {
				for attr {
					key, val, attr = z.TagAttr()
					if bytes.Equal(key, actionBytes) && bytes.HasSuffix(val, actionSuffixBytes) {
						inForm = true
					}
				}
			} else if bytes.Equal(tag, inputBytes) || bytes.Equal(tag, textareaBytes) {
				var name, value string
				for attr {
					key, val, attr = z.TagAttr()
					if bytes.Equal(key, nameBytes) {
						name = string(val)
					} else if bytes.Equal(key, valueBytes) {
						value = string(val)
					} else if bytes.Equal(key, checkedBytes) {
						value = string(val)
					}
				}
				if tag[0] == 't' && tt == html.StartTagToken {
					inTextArea = name
				} else if name != "" {
					form[name] = value
				}
			}
		case html.TextToken:
			if inTextArea != "" {
				form[inTextArea] = form[inTextArea] + string(z.Text())
			}
		case html.EndTagToken:
			tag, _ := z.TagName()
			if bytes.Equal(tag, formBytes) && inForm {
				break loop
			}
			if inTextArea != "" && bytes.Equal(tag, textareaBytes) {
				inTextArea = ""
			}
		}
	}
	if len(form) == 0 {
		return nil, fmt.Errorf("can't parse %s form", actionSuffix)
	}
	return form, nil
}

func jsonStringSlice(v interface{}) []string {
	slice, ok := v.([]interface{})
	if !ok {
		return nil
	}
	res := make([]string, 0, len(slice))
	for _, i := range slice {
		if s, ok := i.(string); ok {
			res = append(res, s)
		}
	}
	return res
}

func jsonString(v interface{}) string {
	s, _ := v.(string)
	return s
}

func jsonBool(v interface{}) bool {
	b, _ := v.(bool)
	return b
}
