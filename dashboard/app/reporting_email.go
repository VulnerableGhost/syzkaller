// Copyright 2017 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package dash

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"text/template"

	"github.com/google/syzkaller/dashboard/dashapi"
	"github.com/google/syzkaller/pkg/email"
	"golang.org/x/net/context"
	"google.golang.org/appengine"
	"google.golang.org/appengine/log"
	aemail "google.golang.org/appengine/mail"
)

// Email reporting interface.

func init() {
	http.HandleFunc("/email_poll", handleEmailPoll)
	http.HandleFunc("/_ah/mail/", handleIncomingMail)

	mailingLists = make(map[string]bool)
	for _, cfg := range config.Namespaces {
		for _, reporting := range cfg.Reporting {
			if cfg, ok := reporting.Config.(*EmailConfig); ok {
				mailingLists[email.CanonicalEmail(cfg.Email)] = true
			}
		}
	}
}

const emailType = "email"

var mailingLists map[string]bool

type EmailConfig struct {
	Email           string
	Moderation      bool
	MailMaintainers bool
}

func (cfg *EmailConfig) Type() string {
	return emailType
}

func (cfg *EmailConfig) NeedMaintainers() bool {
	return cfg.MailMaintainers
}

func (cfg *EmailConfig) Validate() error {
	if _, err := mail.ParseAddress(cfg.Email); err != nil {
		return fmt.Errorf("bad email address %q: %v", cfg.Email, err)
	}
	if cfg.Moderation && cfg.MailMaintainers {
		return fmt.Errorf("both Moderation and MailMaintainers set")
	}
	return nil
}

// handleEmailPoll is called by cron and sends emails for new bugs, if any.
func handleEmailPoll(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	if err := emailPollBugs(c); err != nil {
		log.Errorf(c, "bug poll failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := emailPollJobs(c); err != nil {
		log.Errorf(c, "job poll failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte("OK"))
}

func emailPollBugs(c context.Context) error {
	reports := reportingPoll(c, emailType)
	for _, rep := range reports {
		if err := emailReport(c, rep, "mail_bug.txt"); err != nil {
			log.Errorf(c, "failed to report bug: %v", err)
			continue
		}
		cmd := &dashapi.BugUpdate{
			ID:         rep.ID,
			Status:     dashapi.BugStatusOpen,
			ReproLevel: dashapi.ReproLevelNone,
		}
		if len(rep.ReproC) != 0 {
			cmd.ReproLevel = dashapi.ReproLevelC
		} else if len(rep.ReproSyz) != 0 {
			cmd.ReproLevel = dashapi.ReproLevelSyz
		}
		ok, reason, err := incomingCommand(c, cmd)
		if !ok || err != nil {
			log.Errorf(c, "failed to update reported bug: ok=%v reason=%v err=%v", ok, reason, err)
		}
	}
	return nil
}

func emailPollJobs(c context.Context) error {
	jobs, err := pollCompletedJobs(c, emailType)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if err := emailReport(c, job, "mail_test_result.txt"); err != nil {
			log.Errorf(c, "failed to report job: %v", err)
			continue
		}
		if err := jobReported(c, job.JobID); err != nil {
			log.Errorf(c, "failed to mark job reported: %v", err)
			continue
		}
	}
	return nil
}

func emailReport(c context.Context, rep *dashapi.BugReport, templ string) error {
	cfg := new(EmailConfig)
	if err := json.Unmarshal(rep.Config, cfg); err != nil {
		return fmt.Errorf("failed to unmarshal email config: %v", err)
	}
	to := []string{cfg.Email}
	if cfg.MailMaintainers {
		to = append(to, rep.Maintainers...)
	}
	to = email.MergeEmailLists(to, rep.CC)
	attachments := []aemail.Attachment{
		{
			Name: "config.txt",
			Data: rep.KernelConfig,
		},
	}
	if len(rep.Patch) != 0 {
		attachments = append(attachments, aemail.Attachment{
			Name: "patch.txt",
			Data: rep.Patch,
		})
	}
	if len(rep.Log) != 0 {
		attachments = append(attachments, aemail.Attachment{
			Name: "raw.log",
			Data: rep.Log,
		})
	}
	if len(rep.ReproSyz) != 0 {
		attachments = append(attachments, aemail.Attachment{
			Name: "repro.txt",
			Data: rep.ReproSyz,
		})
	}
	if len(rep.ReproC) != 0 {
		attachments = append(attachments, aemail.Attachment{
			Name: "repro.c",
			Data: rep.ReproC,
		})
	}
	from, err := email.AddAddrContext(fromAddr(c), rep.ID)
	if err != nil {
		return err
	}
	// Data passed to the template.
	type BugReportData struct {
		First        bool
		Moderation   bool
		Maintainers  []string
		CompilerID   string
		KernelRepo   string
		KernelBranch string
		KernelCommit string
		CrashTitle   string
		Report       []byte
		Error        []byte
		HasLog       bool
		ReproSyz     bool
		ReproC       bool
	}
	data := &BugReportData{
		First:        rep.First,
		Moderation:   cfg.Moderation,
		Maintainers:  rep.Maintainers,
		CompilerID:   rep.CompilerID,
		KernelRepo:   rep.KernelRepo,
		KernelBranch: rep.KernelBranch,
		KernelCommit: rep.KernelCommit,
		CrashTitle:   rep.CrashTitle,
		Report:       rep.Report,
		Error:        rep.Error,
		HasLog:       len(rep.Log) != 0,
		ReproSyz:     len(rep.ReproSyz) != 0,
		ReproC:       len(rep.ReproC) != 0,
	}
	log.Infof(c, "sending email %q to %q", rep.Title, to)
	err = sendMailTemplate(c, rep.Title, from, to, rep.ExtID, attachments, templ, data)
	if err != nil {
		return err
	}
	return nil
}

// handleIncomingMail is the entry point for incoming emails.
func handleIncomingMail(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	if err := incomingMail(c, r); err != nil {
		log.Errorf(c, "%v", err)
	}
}

func incomingMail(c context.Context, r *http.Request) error {
	msg, err := email.Parse(r.Body, fromAddr(c))
	if err != nil {
		return err
	}
	log.Infof(c, "received email: subject %q, from %q, cc %q, msg %q, bug %q, cmd %q, link %q",
		msg.Subject, msg.From, msg.Cc, msg.MessageID, msg.BugID, msg.Command, msg.Link)
	// A mailing list can send us a duplicate email, to not process/reply to such duplicate emails,
	// we ignore emails coming from our mailing lists. We could ignore only the mailing list
	// associated with the bug, but we don't know the bug reporting yet (we only have bug ID).
	if msg.Command != "" && mailingLists[email.CanonicalEmail(msg.From)] {
		log.Infof(c, "duplicate email from mailing list, ignoring")
		return nil
	}
	// TODO(dvyukov): check that our mailing list is in CC
	// (otherwise there will be no history of what hsppened with a bug).
	cmd := &dashapi.BugUpdate{
		ID:    msg.BugID,
		ExtID: msg.MessageID,
		Link:  msg.Link,
		CC:    msg.Cc,
	}
	switch msg.Command {
	case "":
		cmd.Status = dashapi.BugStatusUpdate
	case "upstream":
		cmd.Status = dashapi.BugStatusUpstream
	case "invalid":
		cmd.Status = dashapi.BugStatusInvalid
	case "fix:":
		if msg.CommandArgs == "" {
			return replyTo(c, msg, fmt.Sprintf("no commit title"), nil)
		}
		cmd.Status = dashapi.BugStatusOpen
		cmd.FixCommits = []string{msg.CommandArgs}
	case "dup:":
		if msg.CommandArgs == "" {
			return replyTo(c, msg, fmt.Sprintf("no dup title"), nil)
		}
		cmd.Status = dashapi.BugStatusDup
		cmd.DupOf = msg.CommandArgs
	case "test:":
		// TODO(dvyukov): remember email link for jobs.
		if !appengine.IsDevAppServer() {
			return replyTo(c, msg, "testing is experimental", nil)
		}
		args := strings.Split(msg.CommandArgs, " ")
		if len(args) != 2 {
			return replyTo(c, msg, fmt.Sprintf("want 2 args (repo, branch), got %v", len(args)), nil)
		}
		reply := handleTestRequest(c, msg.BugID, email.CanonicalEmail(msg.From),
			msg.MessageID, msg.Patch, args[0], args[1])
		if reply != "" {
			return replyTo(c, msg, reply, nil)
		}
		return nil
	default:
		return replyTo(c, msg, fmt.Sprintf("unknown command %q", msg.Command), nil)
	}
	ok, reply, err := incomingCommand(c, cmd)
	if err != nil {
		return nil // the error was already logged
	}
	if !ok && reply != "" {
		return replyTo(c, msg, reply, nil)
	}
	return nil
}

var mailTemplates = template.Must(template.New("").ParseGlob("mail_*.txt"))

func sendMailTemplate(c context.Context, subject, from string, to []string, replyTo string,
	attachments []aemail.Attachment, template string, data interface{}) error {
	body := new(bytes.Buffer)
	if err := mailTemplates.ExecuteTemplate(body, template, data); err != nil {
		return fmt.Errorf("failed to execute %v template: %v", template, err)
	}
	msg := &aemail.Message{
		Sender:      from,
		To:          to,
		Subject:     subject,
		Body:        body.String(),
		Attachments: attachments,
	}
	if replyTo != "" {
		msg.Headers = mail.Header{"In-Reply-To": []string{replyTo}}
	}
	return sendEmail(c, msg)
}

func replyTo(c context.Context, msg *email.Email, reply string, attachment *aemail.Attachment) error {
	var attachments []aemail.Attachment
	if attachment != nil {
		attachments = append(attachments, *attachment)
	}
	from, err := email.AddAddrContext(fromAddr(c), msg.BugID)
	if err != nil {
		return err
	}
	log.Infof(c, "sending reply: to=%q cc=%q subject=%q reply=%q",
		msg.From, msg.Cc, msg.Subject, reply)
	replyMsg := &aemail.Message{
		Sender:      from,
		To:          []string{msg.From},
		Cc:          msg.Cc,
		Subject:     msg.Subject,
		Body:        email.FormReply(msg.Body, reply),
		Attachments: attachments,
		Headers:     mail.Header{"In-Reply-To": []string{msg.MessageID}},
	}
	return sendEmail(c, replyMsg)
}

// Sends email, can be stubbed for testing.
var sendEmail = func(c context.Context, msg *aemail.Message) error {
	if err := aemail.Send(c, msg); err != nil {
		return fmt.Errorf("failed to send email: %v", err)
	}
	return nil
}

func fromAddr(c context.Context) string {
	return fmt.Sprintf("\"syzbot\" <bot@%v.appspotmail.com>", appengine.AppID(c))
}
