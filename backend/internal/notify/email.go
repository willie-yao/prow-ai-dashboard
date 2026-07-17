package notify

import (
	"bytes"
	"fmt"
	"html/template"
	"net/mail"
	"net/url"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/textutil"
)

type eventKind int

const (
	eventNewFailure eventKind = iota
	eventChangedFailure
)

type emailView struct {
	Heading          string
	ProjectName      string
	TestName         string
	JobName          string
	ConsecutiveCount int
	FailureMessage   string
	AIText           string
	DashboardURL     string
	ProwURL          string
	Recovery         bool
}

var emailHTMLTemplate = template.Must(template.New("notification").Parse(`<!doctype html>
<html>
<body>
  <h2>{{.Heading}}</h2>
  <p><strong>Project:</strong> {{.ProjectName}}</p>
  <table>
    <tr><td><strong>Test</strong></td><td>{{.TestName}}</td></tr>
    <tr><td><strong>Job</strong></td><td>{{.JobName}}</td></tr>
    <tr><td><strong>Status</strong></td><td>{{if .Recovery}}Recovered after {{.ConsecutiveCount}} consecutive failures{{else}}Failed {{.ConsecutiveCount}} consecutive times{{end}}</td></tr>
  </table>
  {{if .FailureMessage}}<h3>Latest error</h3><pre>{{.FailureMessage}}</pre>{{end}}
  {{if .AIText}}<h3>AI analysis</h3><p>{{.AIText}}</p>{{end}}
  <p><a href="{{.DashboardURL}}">View on dashboard</a>{{if .ProwURL}} | <a href="{{.ProwURL}}">View in Prow</a>{{end}}</p>
</body>
</html>`))

func (n *Notifier) failureMessage(kind eventKind, tf models.TestFlakiness, aiSummary, aiRootCause string) Message {
	heading := "Persistent test failure"
	subjectLabel := "Persistent failure"
	if kind == eventChangedFailure {
		heading = "Persistent test failure changed"
		subjectLabel = "Failure changed"
	}

	failureMessage := ""
	prowURL := ""
	if tf.LastFailure != nil {
		failureMessage = textutil.Truncate(tf.LastFailure.FailureMessage, 200)
		if tf.LastFailure.BuildID != "" {
			prowURL = n.prowURLBase + tf.JobName + "/" + tf.LastFailure.BuildID
		}
	}
	aiText := aiRootCause
	if aiText == "" {
		aiText = aiSummary
	}
	if aiText == "" {
		aiText = "No AI analysis available"
	}
	aiText = textutil.Truncate(aiText, 500)

	view := emailView{
		Heading:          heading,
		ProjectName:      n.projectName,
		TestName:         tf.TestName,
		JobName:          tf.JobName,
		ConsecutiveCount: tf.ConsecutiveFailures,
		FailureMessage:   failureMessage,
		AIText:           aiText,
		DashboardURL:     n.dashboardURL(tf.JobName, tf.TestName),
		ProwURL:          prowURL,
	}
	return Message{
		From:     n.from,
		To:       append([]mail.Address(nil), n.to...),
		Subject:  notificationSubject(n.projectName, subjectLabel, tf.TestName),
		TextBody: failureText(view),
		HTMLBody: renderHTML(view),
	}
}

func (n *Notifier) recoveryMessage(nf NotifiedFailure) Message {
	view := emailView{
		Heading:          "Test recovery",
		ProjectName:      n.projectName,
		TestName:         nf.TestName,
		JobName:          nf.JobName,
		ConsecutiveCount: nf.ConsecutiveCount,
		DashboardURL:     n.dashboardURL(nf.JobName, nf.TestName),
		Recovery:         true,
	}
	return Message{
		From:     n.from,
		To:       append([]mail.Address(nil), n.to...),
		Subject:  notificationSubject(n.projectName, "Recovered", nf.TestName),
		TextBody: recoveryText(view),
		HTMLBody: renderHTML(view),
	}
}

func failureText(view emailView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nProject: %s\nTest: %s\nJob: %s\nStatus: Failed %d consecutive times\n",
		view.Heading, view.ProjectName, view.TestName, view.JobName, view.ConsecutiveCount)
	if view.FailureMessage != "" {
		fmt.Fprintf(&b, "\nLatest error:\n%s\n", view.FailureMessage)
	}
	fmt.Fprintf(&b, "\nAI analysis:\n%s\n\nDashboard: %s\n", view.AIText, view.DashboardURL)
	if view.ProwURL != "" {
		fmt.Fprintf(&b, "Prow: %s\n", view.ProwURL)
	}
	return b.String()
}

func recoveryText(view emailView) string {
	return fmt.Sprintf("%s\n\nProject: %s\nTest: %s\nJob: %s\nStatus: Recovered after %d consecutive failures\n\nDashboard: %s\n",
		view.Heading, view.ProjectName, view.TestName, view.JobName, view.ConsecutiveCount, view.DashboardURL)
}

func renderHTML(view emailView) string {
	var b bytes.Buffer
	if err := emailHTMLTemplate.Execute(&b, view); err != nil {
		return ""
	}
	return b.String()
}

func notificationSubject(projectName, label, testName string) string {
	subject := fmt.Sprintf("[%s] %s: %s", cleanHeader(projectName), label, cleanHeader(testName))
	return textutil.Truncate(subject, 180)
}

func cleanHeader(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")), " ")
}

func (n *Notifier) dashboardURL(jobName, testName string) string {
	return fmt.Sprintf("%s/job/%s/test/%s", n.dashboardBaseURL, url.PathEscape(jobName), url.PathEscape(testName))
}

// ParseAddresses parses sender and recipient configuration.
func ParseAddresses(from string, recipients []string) (mail.Address, []mail.Address, error) {
	parsedFrom, err := mail.ParseAddress(from)
	if err != nil {
		return mail.Address{}, nil, fmt.Errorf("parsing email sender: %w", err)
	}
	parsedRecipients := make([]mail.Address, 0, len(recipients))
	for i, recipient := range recipients {
		parsed, err := mail.ParseAddress(recipient)
		if err != nil {
			return mail.Address{}, nil, fmt.Errorf("parsing email recipient %d: %w", i, err)
		}
		parsedRecipients = append(parsedRecipients, *parsed)
	}
	return *parsedFrom, parsedRecipients, nil
}
